package protogen_test

import (
	"os"
	"strings"
	"testing"

	"github.com/ttab/elephantine/test"
	"github.com/ttab/mage/rpc"
)

// TestTwirpModulePins guards the reason the protoc-gen-twirp module exists.
// Its go.mod and go.sum are what make the generated Twirp code a function of
// the pins rather than of the machine, so they have to say the same versions
// github.com/ttab/mage/rpc pins for the fleet, and be compiled by the same
// toolchain. When this fails, regenerate the module:
//
//	cd internal/protogen/twirpgen
//	cp go.mod.txt go.mod && cp go.sum.txt go.sum
//	GOTOOLCHAIN=<rpc.GeneratorToolchain> go mod tidy
//	mv go.mod go.mod.txt && mv go.sum go.sum.txt
func TestTwirpModulePins(t *testing.T) {
	gomod, err := os.ReadFile("twirpgen/go.mod.txt")
	test.Mustf(t, err, "read the protoc-gen-twirp module")

	lines := strings.Split(string(gomod), "\n")

	want := map[string]string{
		"go":                         strings.TrimPrefix(rpc.GeneratorToolchain, "go"),
		"github.com/twitchtv/twirp":  rpc.TwirpVersion + "+incompatible",
		"google.golang.org/protobuf": rpc.ProtocGenGoVersion,
	}

	found := make(map[string]string, len(want))

	for _, line := range lines {
		fields := strings.Fields(strings.TrimSuffix(
			strings.TrimSpace(line), " // indirect"))
		if len(fields) < 2 {
			continue
		}

		if _, ok := want[fields[0]]; !ok {
			continue
		}

		found[fields[0]] = fields[1]
	}

	for name, version := range want {
		test.Equalf(t, version, found[name],
			"pin %s the way github.com/ttab/mage/rpc does", name)
	}
}
