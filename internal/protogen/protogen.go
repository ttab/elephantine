// Package protogen holds the parts of this repository's protobuf generation
// that are code rather than magefile: the module protoc-gen-twirp is run out
// of, and the environment every generator is invoked with.
//
// It exists as a package rather than as magefile code so that the pins it
// carries are covered by a test. github.com/ttab/mage/rpc does the same thing
// for every repository that generates through the rpc namespace; this
// repository cannot use that target, because it lays its protobuf sources out
// as "<dir>/*.proto" rather than as the "<root>/<service>/service.proto" the
// target discovers, so the pieces are mirrored here instead.
package protogen

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ttab/mage/rpc"
)

// The module protoc-gen-twirp is run from, carried as text and written into a
// working directory for the length of a generation run.
//
// The module exists because protoc-gen-twirp is a "+incompatible" module with
// no go.mod of its own: run as "go run <module>@<version>" it resolves
// google.golang.org/protobuf at whatever the proxy answers with that day, and
// it is compiled by whichever toolchain happens to be on the machine. Both
// decide the bytes it writes — the gzipped file descriptor it embeds comes out
// of compress/flate, whose output changed between Go 1.26 and Go 1.27, so the
// same declaration produced a different service.twirp.go on two machines. The
// go directive and the complete go.sum here pin the dependencies,
// rpc.GeneratorToolchain pins the compiler.
//
// Moving rpc.TwirpVersion or rpc.ProtocGenGoVersion means regenerating these
// two files; TestTwirpModulePins fails until they say the same numbers.
var (
	//go:embed twirpgen/go.mod.txt
	twirpGenGoMod string

	//go:embed twirpgen/go.sum.txt
	twirpGenGoSum string
)

// twirpGenName is the tool name the module declares, which is what "go tool"
// is asked for.
const twirpGenName = "protoc-gen-twirp"

// TwirpGenerator writes the protoc-gen-twirp module into the work directory
// and returns the command that runs the plugin out of it. The plugins buf
// spawns take nothing from the directory but the command line, so it only has
// to outlive the buf run.
func TwirpGenerator(work string) ([]string, error) {
	dir := filepath.Join(work, "twirpgen")

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return nil, fmt.Errorf(
			"create the protoc-gen-twirp module directory: %w", err)
	}

	files := map[string]string{
		"go.mod": twirpGenGoMod,
		"go.sum": twirpGenGoSum,
	}

	for name, content := range files {
		err := os.WriteFile(
			filepath.Join(dir, name), []byte(content), 0o600)
		if err != nil {
			return nil, fmt.Errorf(
				"write the protoc-gen-twirp module's %s: %w",
				name, err)
		}
	}

	return []string{"go", "-C", dir, "tool", twirpGenName}, nil
}

// Env returns the environment overrides every generator invocation runs with.
// buf passes its own environment on to the plugins it spawns, so setting it on
// buf is what reaches all of them.
//
// GOTOOLCHAIN pins the compiler, because the toolchain decides some of the
// bytes a generator writes. GOFLAGS has -mod dropped: the generators are
// separate modules run out of the module cache rather than out of this one, so
// a -mod=vendor left in the environment fails every one of them with "cannot
// query module".
func Env() (map[string]string, error) {
	flags, err := goEnv("GOFLAGS")
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"GOTOOLCHAIN": rpc.GeneratorToolchain,
		"GOFLAGS":     withoutModFlag(flags),
	}, nil
}

// goEnv reads one value out of the go command's environment.
func goEnv(name string) (string, error) {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return "", fmt.Errorf("read the go environment %s: %w", name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// withoutModFlag returns a GOFLAGS value with any -mod flag removed.
func withoutModFlag(flags string) string {
	var kept []string

	for f := range strings.FieldsSeq(flags) {
		if strings.HasPrefix(f, "-mod=") {
			continue
		}

		kept = append(kept, f)
	}

	return strings.Join(kept, " ")
}
