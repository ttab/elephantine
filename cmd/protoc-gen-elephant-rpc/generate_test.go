package main_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/ttab/elephantine/test"
)

const (
	// bufVersion, protocGenGoVersion and connectVersion pin the toolchain the
	// golden files were generated with. They are the versions
	// github.com/ttab/mage pins for the fleet; when mage moves, these move
	// with it and the golden files are regenerated.
	bufVersion         = "v1.72.0"
	protocGenGoVersion = "v1.36.12"
	connectVersion     = "v1.20.0"

	// basePackage is the import path of the testdata directory. The fixture
	// is generated into a subdirectory of it per variant, so that the two
	// variants are separate Go packages that can both be compiled.
	basePackage = "github.com/ttab/elephantine/cmd/protoc-gen-elephant-rpc/testdata"

	// moduleRoot is the module root relative to the package directory the
	// tests run in. The plugins are invoked from there, so that
	// "go run ./cmd/protoc-gen-elephant-rpc" resolves.
	moduleRoot = "../.."

	// testdataDir is the fixture directory relative to the module root.
	testdataDir = "cmd/protoc-gen-elephant-rpc/testdata"

	// protoModule is the buf module holding the fixture.
	protoModule = testdataDir + "/proto"

	// streamingModule holds a service with a streaming method, which
	// generation has to reject.
	streamingModule = testdataDir + "/streaming"
)

// goRun is how every generator is started: nothing is installed and nothing
// is taken off PATH, the version in the invocation is the version that
// produced the committed files.
var goRun = []string{"go", "run"}

// generatedSuffixes are the file suffixes the generation owns. A file in the
// fixture that ends with one of them and is not produced by a run is stale.
var generatedSuffixes = []string{
	".pb.go", ".connect.go", ".elephant.go", ".rpc.go",
}

// TestGeneratedFixture generates the fixture with interface=false and
// interface=true and compares the result against the committed files. Run
// with REGENERATE=true to update them.
func TestGeneratedFixture(t *testing.T) {
	variants := []struct {
		name         string
		genInterface bool
	}{
		// The interface comes from protoc-gen-twirp in this variant, see
		// testdata/nointerface/testrpc/v1/twirp_interface.go.
		{name: "nointerface", genInterface: false},
		{name: "withinterface", genInterface: true},
	}

	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			out := t.TempDir()

			generateFixture(t, out, variant.name, variant.genInterface)
			compareFixture(t, out, variant.name)
		})
	}
}

// TestGeneratedFixtureVets compiles the generated fixture packages, which is
// what proves that the handler adapter satisfies the connect-go handler
// interface and that the client adapter satisfies the plain interface.
func TestGeneratedFixtureVets(t *testing.T) {
	var packages []string

	for _, variant := range []string{"nointerface", "withinterface"} {
		dir := "./" + testdataDir + "/" + variant + "/testrpc/v1"

		// Named explicitly rather than with "...": the go command skips
		// directories called testdata when it expands a wildcard.
		packages = append(packages, dir, dir+"/testrpcv1connect")
	}

	runCommand(t, slices.Concat([]string{"go", "vet"}, packages)...)
}

// TestStreamingIsRejected checks that a streaming method fails generation with
// an error that names the method.
func TestStreamingIsRejected(t *testing.T) {
	out := t.TempDir()

	template := generationTemplate([]plugin{{
		Local: slices.Concat(goRun, []string{"./cmd/protoc-gen-elephant-rpc"}),
		Out:   out,
	}})

	output, err := command(bufArgs(template, "./"+streamingModule)).CombinedOutput()
	if err == nil {
		t.Fatalf("generation succeeded for a streaming service:\n%s", output)
	}

	want := "streamrpc.v1.Events.Follow is a streaming RPC"
	if !strings.Contains(string(output), want) {
		t.Fatalf("want an error containing %q, got:\n%s", want, output)
	}
}

// TestConnectVersionPin checks that the connect-go version the fixture is
// generated with is the one the module builds against, so that the adapters
// are compiled against the code they were generated for.
func TestConnectVersionPin(t *testing.T) {
	want := strings.TrimPrefix(connectVersion, "v")

	if connect.Version != want {
		t.Fatalf("the fixture is generated with connect-go %s, but the module builds against %s",
			want, connect.Version)
	}
}

// plugin is one entry in the buf generation template.
type plugin struct {
	Local []string `json:"local"`
	Out   string   `json:"out"`
	Opt   []string `json:"opt,omitempty"`
}

// generationTemplate builds the inline buf template. Nothing is installed and
// nothing is taken off PATH: every plugin is a "go run" at a pinned version,
// or the plugin in this directory.
func generationTemplate(plugins []plugin) string {
	template := struct {
		Version string   `json:"version"`
		Plugins []plugin `json:"plugins"`
	}{
		Version: "v2",
		Plugins: plugins,
	}

	data, err := json.Marshal(template)
	if err != nil {
		panic(fmt.Errorf("marshal generation template: %w", err))
	}

	return string(data)
}

// generateFixture runs buf against the fixture protos, writing the result to
// the out directory.
func generateFixture(t *testing.T, out string, variant string, genInterface bool) {
	t.Helper()

	// The go_package option in the fixture protos is overridden so that the
	// two variants become separate Go packages, and module= strips the
	// testdata import path so that the files land in "<variant>/testrpc/v1".
	opt := []string{"module=" + basePackage}

	for _, name := range []string{"service", "types"} {
		opt = append(opt, fmt.Sprintf(
			"Mtestrpc/v1/%s.proto=%s/%s/testrpc/v1;testrpcv1",
			name, basePackage, variant))
	}

	elephantOpt := slices.Concat(opt, []string{
		fmt.Sprintf("interface=%v", genInterface),
	})

	template := generationTemplate([]plugin{
		{
			Local: slices.Concat(goRun, []string{
				"google.golang.org/protobuf/cmd/protoc-gen-go@" + protocGenGoVersion,
			}),
			Out: out,
			Opt: opt,
		},
		{
			Local: slices.Concat(goRun, []string{
				"connectrpc.com/connect/cmd/protoc-gen-connect-go@" + connectVersion,
			}),
			Out: out,
			Opt: opt,
		},
		{
			Local: slices.Concat(goRun, []string{"./cmd/protoc-gen-elephant-rpc"}),
			Out:   out,
			Opt:   elephantOpt,
		},
	})

	runCommand(t, bufArgs(template, "./"+protoModule)...)
}

// compareFixture compares a generated tree against the committed fixture, and
// replaces the committed files when REGENERATE is set.
func compareFixture(t *testing.T, out string, variant string) {
	t.Helper()

	var (
		got  = collectFiles(t, out, nil)
		want = collectFiles(t, filepath.Join("testdata", variant), generatedSuffixes)
	)

	// The generated files are written relative to the out directory as
	// "<variant>/...", while the committed ones are collected relative to the
	// variant directory.
	for i := range got {
		got[i] = strings.TrimPrefix(got[i], variant+string(filepath.Separator))
	}

	slices.Sort(got)
	slices.Sort(want)

	if test.Regenerate() {
		regenerateFixture(t, out, variant, got, want)

		return
	}

	for _, name := range want {
		if !slices.Contains(got, name) {
			t.Errorf("stale file %q in the %s fixture, run with REGENERATE=true",
				name, variant)
		}
	}

	for _, name := range got {
		if !slices.Contains(want, name) {
			t.Errorf("missing file %q in the %s fixture, run with REGENERATE=true",
				name, variant)

			continue
		}

		var (
			gotData  = readFile(t, filepath.Join(out, variant, name))
			wantData = readFile(t, filepath.Join("testdata", variant, name))
		)

		if gotData != wantData {
			t.Errorf("%s/%s does not match the generated output, run with REGENERATE=true",
				variant, name)
		}
	}
}

func regenerateFixture(t *testing.T, out string, variant string, got, want []string) {
	t.Helper()

	for _, name := range want {
		if slices.Contains(got, name) {
			continue
		}

		err := os.Remove(filepath.Join("testdata", variant, name))
		if err != nil {
			t.Fatalf("remove stale fixture file: %v", err)
		}
	}

	for _, name := range got {
		target := filepath.Join("testdata", variant, name)

		err := os.MkdirAll(filepath.Dir(target), 0o700)
		if err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}

		err = os.WriteFile(target,
			[]byte(readFile(t, filepath.Join(out, variant, name))), 0o600)
		if err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
	}
}

// collectFiles lists the files under dir, relative to it. With suffixes given
// only files with one of those suffixes are listed.
func collectFiles(t *testing.T, dir string, suffixes []string) []string {
	t.Helper()

	var files []string

	_, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil
	}

	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if suffixes != nil && !hasAnySuffix(p, suffixes) {
			return nil
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return fmt.Errorf("relative path of %q: %w", p, err)
		}

		files = append(files, rel)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", dir, err)
	}

	return files
}

func hasAnySuffix(name string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}

	return false
}

func readFile(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}

	return string(data)
}

// bufArgs is the command line that compiles a buf module with an inline
// generation template.
func bufArgs(template string, input string) []string {
	return slices.Concat(goRun, []string{
		"github.com/bufbuild/buf/cmd/buf@" + bufVersion,
		"generate", "--template", template, input,
	})
}

// command builds a command that runs from the module root.
func command(args []string) *exec.Cmd {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = moduleRoot

	return cmd
}

// runCommand runs a command from the module root and fails the test with its
// output if it does not succeed.
func runCommand(t *testing.T, args ...string) {
	t.Helper()

	output, err := command(args).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
