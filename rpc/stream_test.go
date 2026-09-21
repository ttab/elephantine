package rpc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/internal/testservice"
	"github.com/ttab/elephantine/internal/testservice/testserviceconnect"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

// streamImpl implements the streaming fixture service, one method per stream
// type. It is written against connect-go's own handler interface, which is
// what a Connect native service implements.
type streamImpl struct{}

// Emit sends the requested number of messages and then does whatever the
// request asked for: fail with a code, stay open until its context is done, or
// return.
func (streamImpl) Emit(
	ctx context.Context,
	req *connect.Request[testservice.EmitRequest],
	stream *connect.ServerStream[testservice.EmitResponse],
) error {
	if req.Msg.GetTokenExpiry() {
		// Ends the stream when the caller's token expires, so that a
		// long subscription cannot outlive the authorization that
		// opened it.
		expiring, cancel := rpc.ContextWithTokenExpiry(ctx)
		defer cancel()

		ctx = expiring
	}

	for i := range int(req.Msg.GetCount()) {
		err := stream.Send(&testservice.EmitResponse{
			Number: int32(i + 1),
		})
		if err != nil {
			return fmt.Errorf("send the message: %w", err)
		}
	}

	failCode := req.Msg.GetFailCode()
	if failCode != "" {
		var code connect.Code

		err := code.UnmarshalText([]byte(failCode))
		if err != nil {
			return rpc.InvalidArgument("fail_code", "is not an RPC code")
		}

		return rpc.WithMeta(
			rpc.Errorf(code, "the stream failed on purpose"),
			"reason", "the test asked for it")
	}

	if !req.Msg.GetBlock() {
		return nil
	}

	<-ctx.Done()

	// Deliberately the bare cancellation rather than context.Cause: it is
	// the shape a handler is most likely to have, and the code the client
	// is answered with must not depend on which of the two it returns.
	return ctx.Err()
}

// Collect counts the messages and the bytes it is sent.
func (streamImpl) Collect(
	_ context.Context,
	stream *connect.ClientStream[testservice.CollectRequest],
) (*connect.Response[testservice.CollectResponse], error) {
	var res testservice.CollectResponse

	for stream.Receive() {
		res.Messages++
		res.Bytes += int64(len(stream.Msg().GetPayload()))
	}

	err := stream.Err()
	if err != nil {
		return nil, err //nolint:wrapcheck // the test asserts on the code.
	}

	return connect.NewResponse(&res), nil
}

// Exchange echoes every message it is sent.
func (streamImpl) Exchange(
	_ context.Context,
	stream *connect.BidiStream[
		testservice.ExchangeRequest, testservice.ExchangeResponse],
) error {
	for {
		req, err := stream.Receive()

		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err //nolint:wrapcheck // the test asserts on the code.
		}

		err = stream.Send(&testservice.ExchangeResponse{
			Message: req.GetMessage(),
		})
		if err != nil {
			return fmt.Errorf("send the message: %w", err)
		}
	}
}

// emit calls the server-streaming fixture method and returns the messages it
// received, together with the error the caller ends up with, which for a
// stream is the one the receive loop reports.
func emit(
	ctx context.Context,
	client testserviceconnect.StreamServiceClient,
	req *testservice.EmitRequest,
) ([]int32, error) {
	stream, err := client.Emit(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err //nolint:wrapcheck // the test asserts on the code.
	}

	defer func() {
		_ = stream.Close()
	}()

	var messages []int32

	for stream.Receive() {
		messages = append(messages, stream.Msg().GetNumber())
	}

	err = stream.Err()
	if err != nil {
		return messages, err //nolint:wrapcheck // the test asserts on the code.
	}

	return messages, nil
}

// TestStreamMessageLimits checks that a Connect mount is bounded per message
// and not over the life of the request: a client stream that carries more than
// the limit in total succeeds, and a single message over it is refused.
//
// The stream-level limit is set below what the stream carries in total, so a
// stream that succeeds is a stream the APIServer did not put its
// http.MaxBytesReader on.
func TestStreamMessageLimits(t *testing.T) {
	const (
		messageBytes = 1024
		messages     = 8
	)

	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired,
		withMessageBytes(messageBytes),
		withServerOptions(
			elephantine.APIServerMaxBodyBytes(2*messageBytes)))

	client := s.StreamClient(s.Token)

	t.Run("more_than_the_limit_in_total", func(t *testing.T) {
		stream := client.Collect(t.Context())

		for range messages {
			err := stream.Send(&testservice.CollectRequest{
				Payload: make([]byte, messageBytes/2),
			})
			test.Mustf(t, err, "send a message")
		}

		res, err := stream.CloseAndReceive()
		test.Mustf(t, err, "close the client stream")

		test.Equalf(t, int64(messages), res.Msg.GetMessages(),
			"count every message")
		test.Equalf(t, int64(messages*messageBytes/2), res.Msg.GetBytes(),
			"count more bytes than one message may carry")
	})

	t.Run("one_message_over_the_limit", func(t *testing.T) {
		stream := client.Collect(t.Context())

		sendErr := stream.Send(&testservice.CollectRequest{
			Payload: make([]byte, 2*messageBytes),
		})

		_, err := stream.CloseAndReceive()
		if err == nil {
			err = sendErr
		}

		test.IsRPCError(t, err, connect.CodeResourceExhausted)
	})
}

// TestStreamMetrics checks that a stream is observed in
// rpc_stream_duration_seconds and not in rpc_duration_seconds, whose top
// bucket is about thirty seconds, and that rpc_streams_active rises while the
// stream is open and falls when it ends.
func TestStreamMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	stream, err := s.StreamClient(s.Token).Emit(ctx,
		connect.NewRequest(&testservice.EmitRequest{
			Count: 1,
			Block: true,
		}))
	test.Mustf(t, err, "open the stream")

	if !stream.Receive() {
		test.Mustf(t, stream.Err(), "receive the first message")
	}

	err = gatherAndCompare(s.Registry, `
# TYPE rpc_streams_active gauge
rpc_streams_active{method="Emit",service="StreamService"} 1
`, "rpc_streams_active")
	test.Mustf(t, err, "count the open stream")

	// Ending the call from the client side is what an abandoned
	// subscription looks like, and the gauge has to fall for it too.
	cancel()

	_ = stream.Close()

	waitFor(t, "the stream to close", func() bool {
		return gatherValue(t, s, "rpc_streams_active") == 0
	})

	err = gatherAndCompare(s.Registry, `
# TYPE rpc_streams_active gauge
rpc_streams_active{method="Emit",service="StreamService"} 0
`, "rpc_streams_active")
	test.Mustf(t, err, "count the stream out again")

	count, err := testutil.GatherAndCount(s.Registry,
		"rpc_stream_duration_seconds")
	test.Mustf(t, err, "gather the stream duration")

	test.Equalf(t, 1, count, "observe the stream in rpc_stream_duration_seconds")

	count, err = testutil.GatherAndCount(s.Registry, "rpc_duration_seconds")
	test.Mustf(t, err, "gather the unary duration")

	test.Equalf(t, 0, count,
		"keep the stream out of rpc_duration_seconds")

	// The request and the response are still counted, at open and at
	// close, which is the right reading of both: one request, one
	// response, and the response's code is whatever ended the stream.
	err = gatherAndCompare(s.Registry, `
# TYPE rpc_requests_total counter
rpc_requests_total{customer="",method="Emit",service="StreamService"} 1
`, "rpc_requests_total")
	test.Mustf(t, err, "count the stream as a request")
}

// TestStreamDrain checks that a stream is ended with unavailable when the
// server starts shutting down, rather than being severed when the shutdown
// deadline expires. The client is told to reconnect; a truncated stream with
// no code tells it nothing.
func TestStreamDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired,
		withStackContext(ctx))

	stream, err := s.StreamClient(s.Token).Emit(t.Context(),
		connect.NewRequest(&testservice.EmitRequest{
			Count: 1,
			Block: true,
		}))
	test.Mustf(t, err, "open the stream")

	defer func() {
		_ = stream.Close()
	}()

	if !stream.Receive() {
		test.Mustf(t, stream.Err(), "receive the first message")
	}

	// The API server closes the drain when its context is done, before it
	// asks the listeners to shut down.
	cancel()

	test.Equalf(t, false, stream.Receive(),
		"end the stream when the server drains")

	test.IsRPCError(t, stream.Err(), connect.CodeUnavailable)

	// The drain interceptor runs innermost, so the metrics interceptor
	// counts the code the stream actually ended with.
	err = gatherAndCompare(s.Registry, `
# TYPE rpc_protocol_responses_total counter
rpc_protocol_responses_total{client_id="",code="unavailable",method="Emit",protocol="connect",service="StreamService"} 1
`, "rpc_protocol_responses_total")
	test.Mustf(t, err, "count the drained stream as unavailable")
}

// TestStreamTokenExpiry checks that a handler that opted into
// rpc.ContextWithTokenExpiry has its stream ended with unauthenticated when
// the caller's token expires, so that the client reconnects with a fresh token
// rather than treating it as a server fault.
func TestStreamTokenExpiry(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	claims := test.Claims(t, "hugo", "test_read")
	claims.ExpiresAt = jwt.NewNumericDate(
		time.Now().Add(500 * time.Millisecond))

	client := s.StreamClient(s.TokenFor(t, claims))

	_, err := emit(t.Context(), client, &testservice.EmitRequest{
		Count:       1,
		Block:       true,
		TokenExpiry: true,
	})

	test.IsRPCError(t, err, connect.CodeUnauthenticated)
}

// TestStreamErrorAfterFirstMessage checks that an error a handler returns once
// it has sent a message reaches the client with its code and its metadata.
// The response status is written with the first message, so the error travels
// in the end-of-stream frame and reaches the receive loop rather than the
// call's error return.
func TestStreamErrorAfterFirstMessage(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	messages, err := emit(t.Context(), s.StreamClient(s.Token),
		&testservice.EmitRequest{
			Count:    1,
			FailCode: codeNotFound,
		})

	test.Equalf(t, 1, len(messages),
		"receive the message the handler sent before it failed")

	test.IsRPCError(t, err, connect.CodeNotFound)

	test.EqualDiff(t, map[string]string{
		"reason": "the test asked for it",
	}, rpc.Meta(err), "carry the error metadata out of the stream")

	// The HTTP status on the wire is 200 for a stream that failed halfway,
	// since it was written with the first message, but rpc_responses_total
	// reports the status the code maps to, the same way it does for a
	// unary call. rpc_protocol_responses_total carries the code itself.
	err = gatherAndCompare(s.Registry, `
# TYPE rpc_responses_total counter
rpc_responses_total{customer="",method="Emit",service="StreamService",status="404"} 1
`, "rpc_responses_total")
	test.Mustf(t, err, "report the status the code maps to")

	err = gatherAndCompare(s.Registry, `
# TYPE rpc_protocol_responses_total counter
rpc_protocol_responses_total{client_id="",code="not_found",method="Emit",protocol="connect",service="StreamService"} 1
`, "rpc_protocol_responses_total")
	test.Mustf(t, err, "report the code the stream ended with")
}

// TestStreamBidirectional checks the bidirectional fixture against the test
// server, which serves unencrypted HTTP/2 and can therefore carry one.
func TestStreamBidirectional(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	client := testserviceconnect.NewStreamServiceClient(
		s.H2Client(s.Token), "http://"+s.Addr)

	stream := client.Exchange(t.Context())

	err := stream.Send(&testservice.ExchangeRequest{Message: echoMessage})
	test.Mustf(t, err, "send a message")

	res, err := stream.Receive()
	test.Mustf(t, err, "receive the echo")

	test.Equalf(t, echoMessage, res.GetMessage(), "echo the message")

	err = stream.CloseRequest()
	test.Mustf(t, err, "close the request side")

	err = stream.CloseResponse()
	test.Mustf(t, err, "close the response side")
}

// gatherValue returns the sum of the samples of a metric, so that a test can
// wait for a gauge to settle.
func gatherValue(t *testing.T, s *stack, name string) float64 {
	t.Helper()

	families, err := s.Registry.Gather()
	test.Mustf(t, err, "gather the metrics")

	var sum float64

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, metric := range family.GetMetric() {
			sum += metric.GetGauge().GetValue()
		}
	}

	return sum
}

// waitFor polls the condition until it holds, since the server side of a
// stream is closed after the client has stopped reading it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(5 * time.Millisecond)
	}
}
