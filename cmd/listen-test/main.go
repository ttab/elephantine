package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
)

func main() {
	err := run(context.Background())
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger := elephantine.SetUpLogger("debug", os.Stderr)
	grace := elephantine.NewGracefulShutdown(logger, 1*time.Second)

	ctx = grace.CancelOnStop(ctx)

	connString := os.Getenv("CONN_STRING")

	dbpool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return fmt.Errorf("unable to create connection pool: %w", err)
	}

	subA := pg.NewFanOut[string]("chan_a")
	subB := pg.NewFanOut[string]("chan_b")

	sub := pg.NewSubscriber(logger, dbpool,
		[]pg.ChannelSubscription{subA, subB},
	)

	go func() {
		err := sub.Run(ctx)
		if err != nil {
			logger.Error("subscriber stopped",
				elephantine.LogKeyError, err,
			)
		}
	}()

	go sendStuff(ctx, logger, dbpool, subA, subB)

	go recieve(ctx, subA)
	go recieve(ctx, subB)

	<-ctx.Done()

	return nil
}

func sendStuff(
	ctx context.Context,
	logger *slog.Logger,
	db pg.DBExec,
	subA *pg.FanOut[string],
	subB *pg.FanOut[string],
) {
	aTick := time.NewTicker(1 * time.Second)
	bTick := time.NewTicker(1250 * time.Millisecond)

	for {
		select {
		case <-ctx.Done():
			return
		case <-aTick.C:
			println("sending a")

			err := subA.Publish(ctx, db, "hello from A")
			if err != nil {
				logger.Error("failed to publish message",
					"channel", "a")
			}
		case <-bTick.C:
			println("sending b")

			err := subB.Publish(ctx, db, "hello from B")
			if err != nil {
				logger.Error("failed to publish message",
					"channel", "b")
			}
		}
	}
}

func recieve(ctx context.Context, sub *pg.FanOut[string]) {
	msgs := make(chan string)

	go sub.ListenAll(ctx, msgs)

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-msgs:
			println(msg)
		}
	}
}
