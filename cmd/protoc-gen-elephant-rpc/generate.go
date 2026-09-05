package main

import (
	"bytes"
	"fmt"
	"go/token"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/compiler/protogen"
)

const (
	connectPackage = protogen.GoImportPath("connectrpc.com/connect")
	contextPackage = protogen.GoImportPath("context")
	httpPackage    = protogen.GoImportPath("net/http")

	// pluginName is used in the generated header. It is a constant rather
	// than os.Args[0] so that the output is the same however the plugin was
	// invoked.
	pluginName = "protoc-gen-elephant-rpc"

	// adapterFilenameExtension puts the adapters next to the
	// "<proto base>.connect.go" file protoc-gen-connect-go writes.
	adapterFilenameExtension = ".elephant.go"

	// interfaceFilenameExtension puts the plain interface next to the
	// "<proto base>.pb.go" file protoc-gen-go writes, where
	// "<proto base>.twirp.go" holds it today.
	interfaceFilenameExtension = ".rpc.go"

	defaultPackageSuffix = "connect"

	// commentWidth leaves room for the "// " prefix.
	commentWidth = 97
)

// generate emits the adapters, and optionally the plain interface, for one
// proto file.
func generate(
	plugin *protogen.Plugin, file *protogen.File,
	packageSuffix string, genInterface bool,
) error {
	if len(file.Services) == 0 {
		return nil
	}

	err := checkUnary(file)
	if err != nil {
		return err
	}

	if genInterface {
		generateInterfaceFile(plugin, file)
	}

	err = generateAdapterFile(plugin, file, packageSuffix)
	if err != nil {
		return err
	}

	return nil
}

// checkUnary rejects streaming methods. The plain interface has no room for a
// stream, and a service that needs one is not a service that can be served
// over both Twirp and Connect.
func checkUnary(file *protogen.File) error {
	for _, service := range file.Services {
		for _, method := range service.Methods {
			if !method.Desc.IsStreamingClient() && !method.Desc.IsStreamingServer() {
				continue
			}

			return fmt.Errorf(
				"%s is a streaming RPC, and %s only supports unary methods",
				method.Desc.FullName(), pluginName)
		}
	}

	return nil
}

// generateInterfaceFile writes the plain service interfaces into the message
// package, with the names, method sets, signatures and doc comments
// protoc-gen-twirp generates, so that it can take over from Twirp without a
// change in any implementation.
func generateInterfaceFile(plugin *protogen.Plugin, file *protogen.File) {
	g := plugin.NewGeneratedFile(
		file.GeneratedFilenamePrefix+interfaceFilenameExtension,
		file.GoImportPath)

	generatePreamble(g, file, file.GoPackageName)

	for _, service := range file.Services {
		generateInterface(g, service)
	}
}

func generateInterface(g *protogen.GeneratedFile, service *protogen.Service) {
	leadingComments(g, service.Comments.Leading)

	g.P("type ", service.GoName, " interface {")

	for i, method := range service.Methods {
		if i > 0 {
			g.P()
		}

		leadingComments(g, method.Comments.Leading)
		g.P(method.GoName, "(",
			contextPackage.Ident("Context"), ", *", method.Input.GoIdent,
			") (*", method.Output.GoIdent, ", error)")
	}

	g.P("}")
	g.P()
}

// generateAdapterFile writes the handler and client adapters into the package
// protoc-gen-connect-go generates into, so that they can use its generated
// handler and client without exporting anything new from it.
func generateAdapterFile(
	plugin *protogen.Plugin, file *protogen.File, packageSuffix string,
) error {
	prefix, packageName, importPath, err := connectOutput(file, packageSuffix)
	if err != nil {
		return err
	}

	g := plugin.NewGeneratedFile(prefix+adapterFilenameExtension, importPath)

	// The message package is only an import when the adapters live in a
	// package of their own.
	if packageSuffix != "" {
		g.Import(file.GoImportPath)
	}

	generatePreamble(g, file, packageName)

	for _, service := range file.Services {
		generateHandlerAdapter(g, file, service)
		generateClientAdapter(g, file, service)
	}

	return nil
}

// connectOutput computes the filename prefix, package name and import path
// that protoc-gen-connect-go generates with, so that the adapters land in the
// same package and directory as the Connect code they wrap.
func connectOutput(
	file *protogen.File, packageSuffix string,
) (string, protogen.GoPackageName, protogen.GoImportPath, error) {
	if packageSuffix == "" {
		return file.GeneratedFilenamePrefix, file.GoPackageName, file.GoImportPath, nil
	}

	if !token.IsIdentifier(packageSuffix) {
		return "", "", "", fmt.Errorf(
			"package_suffix %q is not a valid Go identifier", packageSuffix)
	}

	packageName := file.GoPackageName + protogen.GoPackageName(packageSuffix)
	prefix := filepath.ToSlash(file.GeneratedFilenamePrefix)

	return path.Join(
			path.Dir(prefix), string(packageName), path.Base(prefix),
		), packageName, protogen.GoImportPath(path.Join(
			string(file.GoImportPath), string(packageName),
		)), nil
}

func generateHandlerAdapter(
	g *protogen.GeneratedFile, file *protogen.File, service *protogen.Service,
) {
	var (
		iface       = file.GoImportPath.Ident(service.GoName)
		constructor = "New" + service.GoName + "ServiceHandler"
		adapter     = unexport(service.GoName) + "ServiceHandler"
	)

	wrapComments(g, constructor, " builds an HTTP handler for the ",
		service.Desc.FullName(), " service from an implementation of the plain ",
		iface, " interface, and returns the path to mount it on together with",
		" the handler, just like New", service.GoName, "Handler does.")
	g.P("//")
	wrapComments(g, "Errors from the implementation are passed through",
		" untouched, so an implementation that wants to control the response",
		" code returns a *", connectPackage.Ident("Error"), ".")
	g.P("func ", constructor, "(svc ", iface,
		", opts ...", connectPackage.Ident("HandlerOption"),
		") (string, ", httpPackage.Ident("Handler"), ") {")
	g.P("return New", service.GoName, "Handler(&", adapter, "{svc: svc}, opts...)")
	g.P("}")
	g.P()

	wrapComments(g, adapter, " implements ", service.GoName,
		"Handler on top of a ", iface, ".")
	g.P("type ", adapter, " struct {")
	g.P("svc ", iface)
	g.P("}")
	g.P()

	for _, method := range service.Methods {
		g.P("func (h *", adapter, ") ", method.GoName,
			"(ctx ", contextPackage.Ident("Context"),
			", req *", connectPackage.Ident("Request"), "[", method.Input.GoIdent, "]",
			") (*", connectPackage.Ident("Response"), "[", method.Output.GoIdent, "]",
			", error) {")
		g.P("res, err := h.svc.", method.GoName, "(ctx, req.Msg)")
		g.P("if err != nil {")
		g.P("return nil, err")
		g.P("}")
		g.P()
		g.P("return ", connectPackage.Ident("NewResponse"), "(res), nil")
		g.P("}")
		g.P()
	}
}

func generateClientAdapter(
	g *protogen.GeneratedFile, file *protogen.File, service *protogen.Service,
) {
	var (
		iface       = file.GoImportPath.Ident(service.GoName)
		constructor = "New" + service.GoName + "ServiceClient"
		adapter     = unexport(service.GoName) + "ServiceClient"
	)

	wrapComments(g, constructor, " constructs a client for the ",
		service.Desc.FullName(), " service that implements the plain ", iface,
		" interface, so that it is a drop-in for the Twirp clients. The",
		" options are the ones New", service.GoName, "Client takes.")
	g.P("//")
	wrapComments(g, "The base URL is the base URL of the Connect or gRPC",
		" server, for example https://repository.api.tt.se. Errors are",
		" returned as the *", connectPackage.Ident("Error"),
		" values the Connect client produces.")
	g.P("func ", constructor, "(httpClient ", connectPackage.Ident("HTTPClient"),
		", baseURL string, opts ...", connectPackage.Ident("ClientOption"),
		") ", iface, " {")
	g.P("return &", adapter, "{")
	g.P("client: New", service.GoName, "Client(httpClient, baseURL, opts...),")
	g.P("}")
	g.P("}")
	g.P()

	wrapComments(g, adapter, " implements ", iface, " on top of a ",
		service.GoName, "Client.")
	g.P("type ", adapter, " struct {")
	g.P("client ", service.GoName, "Client")
	g.P("}")
	g.P()

	for _, method := range service.Methods {
		g.P("func (c *", adapter, ") ", method.GoName,
			"(ctx ", contextPackage.Ident("Context"),
			", req *", method.Input.GoIdent,
			") (*", method.Output.GoIdent, ", error) {")
		g.P("res, err := c.client.", method.GoName,
			"(ctx, ", connectPackage.Ident("NewRequest"), "(req))")
		g.P("if err != nil {")
		g.P("return nil, err")
		g.P("}")
		g.P()
		g.P("return res.Msg, nil")
		g.P("}")
		g.P()
	}
}

func generatePreamble(
	g *protogen.GeneratedFile, file *protogen.File,
	packageName protogen.GoPackageName,
) {
	g.P("// Code generated by ", pluginName, ". DO NOT EDIT.")
	g.P("//")
	g.P("// Source: ", file.Desc.Path())
	g.P()
	g.P("package ", packageName)
	g.P()
}

// leadingComments prints the doc comment of a service or method as it stands
// in the proto file, which is what protoc-gen-twirp does.
func leadingComments(g *protogen.GeneratedFile, comments protogen.Comments) {
	if comments.String() == "" {
		return
	}

	g.P(strings.TrimSpace(comments.String()))
}

// wrapComments word-wraps a generated doc comment. Lifted from
// protoc-gen-connect-go so that the two files read the same.
func wrapComments(g *protogen.GeneratedFile, elems ...any) {
	text := &bytes.Buffer{}

	for _, el := range elems {
		switch el := el.(type) {
		case protogen.GoIdent:
			fmt.Fprint(text, g.QualifiedGoIdent(el))
		default:
			fmt.Fprint(text, el)
		}
	}

	words := strings.Fields(text.String())

	text.Reset()

	var pos int

	for _, word := range words {
		numRunes := utf8.RuneCountInString(word)
		if pos > 0 && pos+numRunes+1 > commentWidth {
			g.P("// ", text.String())
			text.Reset()

			pos = 0
		}

		if pos > 0 {
			text.WriteRune(' ')

			pos++
		}

		text.WriteString(word)

		pos += numRunes
	}

	if text.Len() > 0 {
		g.P("// ", text.String())
	}
}

// unexport lowercases the first letter of a generated identifier, avoiding the
// Go keywords. Lifted from protoc-gen-connect-go.
func unexport(s string) string {
	lowercased := strings.ToLower(s[:1]) + s[1:]

	switch lowercased {
	// https://go.dev/ref/spec#Keywords
	case "break", "default", "func", "interface", "select",
		"case", "defer", "go", "map", "struct",
		"chan", "else", "goto", "package", "switch",
		"const", "fallthrough", "if", "range", "type",
		"continue", "for", "import", "return", "var":
		return "_" + lowercased
	default:
		return lowercased
	}
}
