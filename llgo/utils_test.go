package main

import (
	"fmt"
	"github.com/axw/gollvm/llvm"
	"github.com/axw/llgo"
	"go/build"
	"os/exec"
	"path"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func testdata(files ...string) []string {
	for i, f := range files {
		files[i] = "testdata/" + f
	}
	return files
}

func init() {
	llvm.LinkInJIT()
	llvm.InitializeNativeTarget()
}

func readPipe(p int, c chan<- string) {
	var s string
	buf := make([]byte, 4096)
	n, _ := syscall.Read(p, buf)
	for n > 0 {
		s += string(buf[:n])
		n, _ = syscall.Read(p, buf)
	}
	c <- s
}

func addExterns(m *llgo.Module) {
	CharPtr := llvm.PointerType(llvm.Int8Type(), 0)
	fn_type := llvm.FunctionType(
		llvm.Int32Type(), []llvm.Type{CharPtr}, false)
	fflush := llvm.AddFunction(m.Module, "fflush", fn_type)
	fflush.SetFunctionCallConv(llvm.CCallConv)
}

func getRuntimeFiles() (files []string, err error) {
	var pkg *build.Package
	pkgpath := "github.com/axw/llgo/pkg/runtime"
	pkg, err = build.Import(pkgpath, "", 0)
	if err == nil {
		files = make([]string, len(pkg.GoFiles))
		for i, filename := range pkg.GoFiles {
			files[i] = path.Join(pkg.Dir, filename)
		}
	}
	return
}

func getRuntimeModule() (m llvm.Module, err error) {
	gofiles, err := getRuntimeFiles()
	if err == nil {
		var runtimeModule *llgo.Module
		runtimeModule, err = compileFiles(gofiles)
		if runtimeModule != nil {
			m = runtimeModule.Module
		}
	}
	return
}

func addRuntime(m *llgo.Module) (err error) {
	runtimeModule, err := getRuntimeModule()
	if err != nil {
		return
	}
	llvm.LinkModules(m.Module, runtimeModule, llvm.LinkerDestroySource)
	return
}

func runFunction(m *llgo.Module, name string) (output []string, err error) {
	addExterns(m)
	err = addRuntime(m)
	if err != nil {
		return
	}

	err = llvm.VerifyModule(m.Module, llvm.ReturnStatusAction)
	if err != nil {
		return
	}

	engine, err := llvm.NewExecutionEngine(m.Module)
	if err != nil {
		return
	}
	defer engine.Dispose()

	fn := engine.FindFunction(name)
	if fn.IsNil() {
		err = fmt.Errorf("Couldn't find function '%s'", name)
		return
	}

	// Redirect stdout to a pipe.
	pipe_fds := make([]int, 2)
	err = syscall.Pipe(pipe_fds)
	if err != nil {
		return
	}
	defer syscall.Close(pipe_fds[0])
	defer syscall.Close(pipe_fds[1])
	old_stdout, err := syscall.Dup(syscall.Stdout)
	if err != nil {
		return
	}
	defer syscall.Close(old_stdout)
	err = syscall.Dup2(pipe_fds[1], syscall.Stdout)
	if err != nil {
		return
	}
	defer syscall.Dup2(old_stdout, syscall.Stdout)

	c := make(chan string)
	go readPipe(pipe_fds[0], c)

	exec_args := []llvm.GenericValue{}
	engine.RunStaticConstructors()
	engine.RunFunction(fn, exec_args)
	defer engine.RunStaticDestructors()

	// Call fflush to flush stdio (printf), then sync and close the write
	// end of the pipe.
	fflush := engine.FindFunction("fflush")
	ptr0 := unsafe.Pointer(uintptr(0))
	exec_args = []llvm.GenericValue{llvm.NewGenericValueFromPointer(ptr0)}
	engine.RunFunction(fflush, exec_args)
	syscall.Fsync(pipe_fds[1])
	syscall.Close(pipe_fds[1])
	syscall.Close(syscall.Stdout)

	output_str := <-c
	output = strings.Split(strings.TrimSpace(output_str), "\n")
	return
}

func checkStringsEqual(out, expectedOut []string) error {
	if !reflect.DeepEqual(out, expectedOut) {
		return fmt.Errorf("Output did not match: %q (actual) != %q (expected)",
			out, expectedOut)
	}
	return nil
}

func checkStringsEqualUnordered(out, expectedOut []string) error {
	outSorted := make([]string, len(out))
	expectedOutSorted := make([]string, len(expectedOut))
	copy(outSorted, out)
	copy(expectedOutSorted, expectedOut)
	sort.Strings(outSorted)
	sort.Strings(expectedOutSorted)
	if !reflect.DeepEqual(outSorted, expectedOutSorted) {
		return fmt.Errorf("Output did not match: %q (actual) != %q (expected)",
			outSorted, expectedOutSorted)
	}
	return nil
}

func runAndCheckMain(check func(a, b []string) error, files []string) error {
	// First run with "go run" to get the expected output.
	cmd := exec.Command("go", append([]string{"run"}, files...)...)
	gorun_out, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	expected := strings.Split(strings.TrimSpace(string(gorun_out)), "\n")

	// Now compile to and interpret the LLVM bitcode, comparing the output to
	// the output of "go run" above.
	m, err := compileFiles(files)
	if err != nil {
		return err
	}
	output, err := runFunction(m, "main")
	if err == nil {
		err = check(output, expected)
	}
	return err
}

// checkOutputEqual compiles and runs the specified files using gc and llgo,
// and checks that their output matches exactly.
func checkOutputEqual(t *testing.T, files ...string) {
	err := runAndCheckMain(checkStringsEqual, testdata(files...))
	if err != nil {
		t.Fatal(err)
	}
}

// checkOutputEqualUnordered compiles and runs the specified files using gc
// and llgo, and checks that their output, when split by line and sorted,
// matches.
func checkOutputEqualUnordered(t *testing.T, files ...string) {
	err := runAndCheckMain(checkStringsEqualUnordered, testdata(files...))
	if err != nil {
		t.Fatal(err)
	}
}

// vim: set ft=go:
