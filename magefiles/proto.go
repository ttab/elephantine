//go:build mage
// +build mage

package main

import (
	"encoding/json"
	"fmt"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
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
// this repository is laid out that way.
func (Proto) Generate() error {
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
				"local": goRun("github.com/twitchtv/twirp/protoc-gen-twirp",
					rpc.TwirpVersion),
				"out": ".",
				"opt": []string{"paths=source_relative"},
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

	err = sh.RunV("go", append([]string{
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
