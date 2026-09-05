// protoc-gen-elephant-rpc is a protobuf compiler plugin that keeps the plain
// service interface alive on top of Connect. Given a service it emits two
// adapters into the package that protoc-gen-connect-go generates into:
//
//   - New<Service>ServiceHandler(svc <pkg>.<Service>,
//     opts ...connect.HandlerOption) (string, http.Handler) serves an
//     implementation that has the plain interface signatures
//     (ctx, *Request) (*Response, error), the ones protoc-gen-twirp has
//     always generated, over Connect.
//   - New<Service>ServiceClient(httpClient connect.HTTPClient,
//     baseURL string, opts ...connect.ClientOption) <pkg>.<Service> is a
//     client with that same plain interface, so it is a drop-in for
//     New<Service>ProtobufClient.
//
// With the interface=true option it also emits the plain interface itself,
// with the name, method set, signatures and doc comments protoc-gen-twirp
// emits, for the day Twirp generation is switched off.
//
// The emitted code imports connectrpc.com/connect, context, net/http and the
// message package, and nothing else. In particular it never imports
// elephantine, so a declarations module like elephant-api can generate with
// this plugin without taking on elephantine's dependencies.
//
// It is run through buf, at a version pinned by github.com/ttab/mage:
//
//	{
//	  "version": "v2",
//	  "plugins": [
//	    {"local": ["go", "run", "github.com/ttab/elephantine/cmd/protoc-gen-elephant-rpc@v0.29.0"], "out": "."}
//	  ]
//	}
//
// Only unary RPCs are supported; a streaming method fails generation.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

const (
	packageSuffixFlagName = "package_suffix"
	interfaceFlagName     = "interface"

	usage = "protoc-gen-elephant-rpc generates plain-interface adapters for" +
		" Connect services.\n\nFlags:\n" +
		"  -h, --help\tPrint this help and exit.\n" +
		"      --version\tPrint the version and exit."
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stdout, pluginVersion())
		os.Exit(0)
	}

	if len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Fprintln(os.Stdout, usage)
		os.Exit(0)
	}

	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	var flagSet flag.FlagSet

	packageSuffix := flagSet.String(
		packageSuffixFlagName, defaultPackageSuffix,
		"The package suffix protoc-gen-connect-go generates with. The"+
			" adapters are written to the same package, so the two options"+
			" must agree.",
	)

	// The interface option is a string so that it can be set as a bare
	// "interface" without a value, the way connect-go's flags work.
	interfaceOpt := flagSet.String(
		interfaceFlagName, "false",
		"Generate the plain service interface into the message package as"+
			" well. Off while protoc-gen-twirp still generates it.",
	)

	protogen.Options{
		ParamFunc: flagSet.Set,
	}.Run(func(plugin *protogen.Plugin) error {
		plugin.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL) |
			uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)
		plugin.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
		plugin.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2024

		genInterface, err := parseBoolOption(interfaceFlagName, *interfaceOpt)
		if err != nil {
			return err
		}

		for _, file := range plugin.Files {
			if !file.Generate {
				continue
			}

			err := generate(plugin, file, *packageSuffix, genInterface)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// pluginVersion reports the module version the plugin was built from, which
// is the elephantine version when it is run as "go run
// github.com/ttab/elephantine/cmd/protoc-gen-elephant-rpc@<version>".
func pluginVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(unknown)"
	}

	return info.Main.Version
}

// parseBoolOption parses a boolean plugin option that may be given as a bare
// flag name without a value.
func parseBoolOption(name string, value string) (bool, error) {
	switch value {
	case "", "true":
		return true, nil
	case "false":
		return false, nil
	}

	return false, fmt.Errorf(
		"unknown value for option %q (must be one of \"\", \"true\", \"false\"): %q",
		name, value)
}
