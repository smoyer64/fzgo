package fuzz

import (
	"bytes"
	"fmt"
	"go/types"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/thepudds/fzgo/randparam" // TODO: for now, force import to simplify install
	"golang.org/x/tools/imports"
)

// richsig enables fuzzing of rich function signatures with fzgo and dvyukov/go-fuzz beyond
// just func([]byte) int.
//
// For example, without manual work, can fuzz functions like:
//   func FuzzFunc(re string, input []byte, posix bool) (bool, error)

// some examples that work:
//   ./fzgo test -fuzz=. ./examples/richsignatures
//
// this uses all basic types:
//   ./fzgo test ./examples/... -fuzz=FuzzWithBasicTypes
// this uses a non-stdlib type:
//   ./fzgo test ./examples/... -fuzz=FuzzWithFzgoFunc
// this uses goimports right now to set up imports:
//   ./fzgo test ./examples/... -fuzz=FuzzWithStdlibType
//
// check literal injection (works):
//   ./fzgo test ./examples/... -fuzz=FuzzHardToGuessNumber

// IsPlainSig reports whether a signature is a classic, plain 'func([]bytes) int'
// go-fuzz signature.
func IsPlainSig(f *types.Func) (bool, error) {
	sig, ok := f.Type().(*types.Signature)
	if !ok {
		return false, fmt.Errorf("function is not *types.Signature (%+v)", f)
	}
	results := sig.Results()
	params := sig.Params()
	if params.Len() != 1 || results.Len() != 1 {
		return false, nil
	}
	if types.TypeString(params.At(0).Type(), nil) != "[]byte" {
		return false, nil
	}
	if types.TypeString(results.At(0).Type(), nil) != "int" {
		return false, nil
	}
	return true, nil
}

// CreateRichSigWrapper creates a temp working directory, then
// creates a rich signature wrapping fuzz function.
// Important: don't set printArgs=true when actually fuzzing. (Likely bad for perf, though not yet attempted).
func CreateRichSigWrapper(function Func, printArgs bool) (t Target, err error) {
	report := func(err error) (Target, error) {
		return Target{}, fmt.Errorf("creating wrapper function for %s: %v", function.FuzzName(), err)
	}

	// create temp dir to work in
	tempDir, err := ioutil.TempDir("", "fzgo-fuzz-rich-signature")
	if err != nil {
		return report(fmt.Errorf("create staging temp dir: %v", err))
	}
	defer func() {
		// conditionally clean up. (this is a bit of an experiment to use named return err here).
		if err != nil {
			// on our way out, but encountered an error, so delete the temp dir
			os.RemoveAll(tempDir)
		}
	}()

	wrapperDir := filepath.Join(tempDir, "gopath", "src", "richsigwrapper")
	if err := os.MkdirAll(wrapperDir, 0700); err != nil {
		return report(fmt.Errorf("failed to create gopath/src in temp dir: %v", err))
	}

	origGp := Gopath()
	gp := strings.Join([]string{origGp, filepath.Join(tempDir, "gopath")},
		string(os.PathListSeparator))

	// cd to our temp dir to simplify things when we indirectly invoke the
	// 'go' command (e.g., when searching for funcs below).
	oldWd, err := os.Getwd()
	if err != nil {
		return report(err)
	}
	err = os.Chdir(wrapperDir)
	if err != nil {
		return report(err)
	}
	defer func() { os.Chdir(oldWd) }()

	// create our temporary richsigwrapper.go file
	var b bytes.Buffer
	err = createWrapper(&b, function, printArgs)
	if err != nil {
		return report(fmt.Errorf("failed constructing rich signature wrapper: %v", err))
	}

	// fix up any needed imports.
	out, err := imports.Process("richsigwrapper.go", b.Bytes(), nil)
	if err != nil {
		return report(fmt.Errorf("failed adjusting imports: %v", err))
	}

	err = ioutil.WriteFile(filepath.Join(wrapperDir, "richsigwrapper.go"), out, 0700)
	if err != nil {
		return report(fmt.Errorf("failed to create temporary richsigwrapper.go: %v", err))
	}

	// Create an env map to include our temporary gopath.
	// (If env contains duplicate environment keys for GOPATH, only the last value is used).
	env := append(os.Environ(), "GOPATH="+gp)

	// TODO: ##################################################################################
	// TODO: ###########  resume finishing up here, also fuzz.Instrument, fuzz.Start ##########
	// TODO: ##################################################################################

	// Re-use our fuzz.FindFunc to find the newly created wrapper.
	// Note: pkg patterns like 'fzgo/...' and 'fzgo/richsigwrapper' don't seem to work, but '.' does.
	// (We cd'ed above to the working directory. Maybe a go/packages bug, not liking >1 GOPATH entry?)
	functions, err := FindFunc(".", "FuzzRichSigWrapper", env, false)
	if err != nil || len(functions) == 0 {
		return report(fmt.Errorf("failed to find wrapper func in temp gopath: %v", err))
	}

	// Pull together everything we need about our wrapper into a Target.
	// This will be the actual target for go-fuzz-build and go-fuzz,
	// though we track the user's original function so we can send
	// the output to the proper location under the original location if requested,
	// and use the original path for cache computation,
	// as well as show friendly names and more generally mask from the user that
	// we created  a temporary wrapper.
	target := Target{
		UserFunc:       function,
		hasWrapper:     true,
		wrapperFunc:    functions[0],
		wrapperEnv:     env,
		wrapperTempDir: wrapperDir,
	}

	return target, nil
}

func createWrapper(w io.Writer, function Func, printArgs bool) error {
	f := function.TypesFunc
	sig, ok := f.Type().(*types.Signature)
	if !ok {
		return fmt.Errorf("function %s is not *types.Signature (%+v)", function, f)
	}

	// start emitting the wrapper program!
	// TODO: add in something like: fuzzer := gofuzz.New().NilChance(0.1).NumElements(0, 10).MaxDepth(10)
	fmt.Fprintf(w, "\npackage richsigwrapper\n")
	fmt.Fprintf(w, "\nimport \"%s\"\n", function.PkgPath)
	fmt.Fprintf(w, `
import "github.com/thepudds/fzgo/randparam"

// FuzzRichSigWrapper is an automatically generated wrapper that is
// compatible with dvyukov/go-fuzz.
func FuzzRichSigWrapper(data []byte) int {
	fuzzer := randparam.NewFuzzer(data)
	fuzzOne(fuzzer)
	return 0
}

// fuzzOne is an automatically generated function that
// uses fzgo/randparam.Fuzzer to automatically fuzz the arguments for a
// user-supplied function.
func fuzzOne (fuzzer *randparam.Fuzzer) {

	// Create random args for each parameter from the signature.
	// fuzzer.Fuzz recursively fills all of obj's fields with something random.
	// Only exported (public) fields can be set currently. (That is how google/go-fuzz operates).
`)
	// iterate over the parameters, emitting the wrapper function as we go.
	// loosely modeled after PrintHugeParams in https://github.com/golang/example/blob/master/gotypes/hugeparam/main.go#L24
	tuple := sig.Params()
	for i := 0; i < tuple.Len(); i++ {
		v := tuple.At(i)
		// want:
		//		var foo string
		//		fuzzer.Fuzz(&foo)

		typeStringWithSelector := types.TypeString(v.Type(), externalQualifier)
		fmt.Fprintf(w, "\tvar %s %s\n", v.Name(), typeStringWithSelector)
		fmt.Fprintf(w, "\tfuzzer.Fuzz(&%s)\n", v.Name())
		if printArgs {
			fmt.Fprintf(w, "\tfmt.Printf(\"               arg %d:     %%#v\\n\", %s)\n",
				i+1, v.Name())
		}
		fmt.Fprintf(w, "\n")
	}

	// emit the call to the wrapped function
	fmt.Fprintf(w, "\t%s.%s(", f.Pkg().Name(), f.Name()) // was target.%s with f.Name()

	// emit the arguments to the wrapped function
	var names []string
	for i := 0; i < tuple.Len(); i++ {
		v := tuple.At(i)
		names = append(names, v.Name())
	}
	fmt.Fprintf(w, "%s)\n\n}\n", strings.Join(names, ", "))

	return nil
}

// externalQualifier can be used as types.Qualifier in calls to types.TypeString and similar.
func externalQualifier(p *types.Package) string {
	// always return the package name, which
	// should give us things like pkgname.SomeType
	return p.Name()
}
