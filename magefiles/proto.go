//go:build mage
// +build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/ttab/elephantine/internal/protogen"
	"github.com/ttab/mage/rpc"
)

// Proto compiles the protobuf sources in this repository.
type Proto mg.Namespace

// protoPaths are the directories holding protobuf sources. The rpc package
// declares the ErrorMeta detail the error helpers carry metadata in, and the
// testservice package is the fixture the dual-stack tests are run against.
var protoPaths = []string{"rpc", "internal/testservice"}

// Generate compiles the protobuf sources with buf and the plugin versions
// github.com/ttab/mage/rpc pins for the fleet, so that the committed generated
// code moves when the fleet's generators move.
//
// The rpc:generate target in ttab/mage cannot be used here: it discovers
// services as "<proto root>/*/service.proto", and neither of the sources in
// this repository is laid out that way. What that target does to keep
// generation reproducible is mirrored instead: every generator runs under the
// pinned toolchain with -mod out of the way, and protoc-gen-twirp runs out of
// the module in internal/protogen/twirpgen rather than at a bare version,
// since it has no go.mod of its own and would otherwise resolve its
// dependencies afresh on every run.
func (Proto) Generate() error {
	env, err := protogen.Env()
	if err != nil {
		return fmt.Errorf("resolve the generator environment: %w", err)
	}

	// The protoc-gen-twirp module is written here for the length of the
	// run. The plugins buf spawns take nothing from it but the command
	// line, so the directory only has to outlive buf.
	work, err := os.MkdirTemp("", "elephantine-proto-")
	if err != nil {
		return fmt.Errorf(
			"create the generator working directory: %w", err)
	}

	defer func() {
		_ = os.RemoveAll(work)
	}()

	twirp, err := protogen.TwirpGenerator(work)
	if err != nil {
		return fmt.Errorf("write the protoc-gen-twirp module: %w", err)
	}

	template, err := json.Marshal(map[string]any{
		"version": "v2",
		"plugins": []map[string]any{
			{
				"local": goRun("google.golang.org/protobuf/cmd/protoc-gen-go",
					rpc.ProtocGenGoVersion),
				"out": ".",
				"opt": []string{"paths=source_relative"},
			},
			{
				"local": goRun("connectrpc.com/connect/cmd/protoc-gen-connect-go",
					rpc.ConnectGoVersion),
				"out": ".",
				"opt": []string{"paths=source_relative"},
			},
			{
				// The plugin in this repository, run from the
				// checkout rather than at a version, since
				// this is where it is developed.
				"local": []string{"go", "run", "./cmd/protoc-gen-elephant-rpc"},
				"out":   ".",
				"opt":   []string{"paths=source_relative"},
			},
			{
				"local": twirp,
				"out":   ".",
				"opt":   []string{"paths=source_relative"},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal the generation template: %w", err)
	}

	args := []string{"generate", "--template", string(template)}

	for _, p := range protoPaths {
		args = append(args, "--path", p)
	}

	err = sh.RunWithV(env, "go", append([]string{
		"run", "github.com/bufbuild/buf/cmd/buf@" + rpc.BufVersion,
	}, args...)...)
	if err != nil {
		return fmt.Errorf("run buf generate: %w", err)
	}

	return nil
}

// goRun is how every generator is started: nothing is installed and nothing is
// taken off PATH.
func goRun(module string, version string) []string {
	return []string{"go", "run", module + "@" + version}
}
