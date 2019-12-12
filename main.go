package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nvanbenschoten/benchcmp/google"
	"github.com/nvanbenschoten/benchcmp/ui"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"golang.org/x/perf/benchstat"
)

const usage = `usage: benchcmp [--old <commit>] [--new <commit>] <pkgs>...`

const helpString = `benchcmp automates the process of running and comparing Go microbenchmarks
across code changes.

benchcmp runs all microbenchmarks in the specified packages against the old and
new commit. It then passes the benchmark output through benchstat to compute
statistics about the results.

By default, benchcmp outputs these results in a textual format. However, if the
--sheets flag is passed then it will upload the result to a Google Sheets
spreadsheet. To access this, users must have a Google service account. For
information, see https://cloud.google.com/iam/docs/service-accounts.

The Google service account must meet the following conditions:
1. The Google Sheets API must be enabled for the account's project
2. The Google Drive  API must be enabled for the account's project

When the --sheets flag is passed, benchcmp will search for a credentials file
containing the service account key using the GOOGLE_APPLICATION_CREDENTIALS
environment variable. See https://cloud.google.com/docs/authentication/production.

Options:
  -n, --new    <commit> measure the difference between this commit and old (default HEAD)
  -o, --old    <commit> measure the difference between this commit and new (default new~)
  -c, --count  <n>      run tests and benchmarks n times (default 1)
      --post-checkout   an optional command to run after checking out each branch
                        to configure the git repo so that 'go build' succeeds
      --sheets          output the results to a new Google sheets document
      --help            display this help

Example invocations:
  $ benchcmp --sheets ./pkg/...
  $ benchcmp --old=master~ --new=master ./pkg/kv ./pkg/storage/...
  $ benchcmp --new=d1fbdb2 --count=2 ./pkg/sql/...
  $ benchcmp --new=6299bd4 --sheets --post-checkout='make buildshort' ./pkg/workload/...`

// TODO: it's unclear whether G Suite Domain-wide Delegation is required for the
// Google service account. If it is, add the following requirement to the help
// text above.
//   3. G Suite Domain-wide Delegation must be enabled. See
//    https://developers.google.com/identity/protocols/OAuth2ServiceAccount#delegatingauthority.

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	var help, useSheets bool
	var oldRef, newRef, postChck string
	var itersPerTest int

	pflag.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	pflag.BoolVarP(&help, "help", "h", false, "")
	pflag.BoolVarP(&useSheets, "sheets", "", false, "")
	pflag.StringVarP(&oldRef, "old", "o", "", "")
	pflag.StringVarP(&newRef, "new", "n", "", "")
	pflag.StringVarP(&postChck, "post-checkout", "", "", "")
	pflag.IntVarP(&itersPerTest, "count", "c", 10, "")
	pflag.Parse()
	prArgs := pflag.Args()

	if help {
		return runHelp(ctx)
	}
	if len(prArgs) == 0 {
		return runHelp(ctx)
	}
	pkgFilter := prArgs
	sort.Strings(pkgFilter)

	// Parse the specified git refs.
	var err error
	oldRef, newRef, err = parseGitRefs(oldRef, newRef)
	if err != nil {
		return err
	}

	var srv *google.Service
	if useSheets {
		srv, err = google.New(ctx)
		if err != nil {
			return err
		}
	}

	// Build the benchmark suites.
	oldSuite := makeBenchSuite(oldRef)
	newSuite := makeBenchSuite(newRef)
	defer oldSuite.close()
	defer newSuite.close()
	if err := buildBenches(ctx, pkgFilter, postChck, &oldSuite, &newSuite); err != nil {
		return err
	}

	// Run the benchmarks.
	tests := oldSuite.intersectTests(&newSuite)
	err = runbenchcmpes(ctx, &oldSuite, &newSuite, tests.sorted(), itersPerTest)
	if err != nil {
		return err
	}

	// Process the benchmark output.
	return processBenchOutput(ctx, &oldSuite, &newSuite, pkgFilter, srv)
}

func runHelp(ctx context.Context) error {
	fmt.Fprintln(os.Stderr, usage)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, helpString)
	return nil
}

func parseGitRefs(oldRef, newRef string) (string, string, error) {
	var err error
	if newRef == "" {
		newRef, err = getCurRef()
		if err != nil {
			return "", "", err
		}
	}
	newRef = shortenRef(newRef)
	if ok, err := checkValidRef(newRef); err != nil {
		return "", "", err
	} else if !ok {
		return "", "", errors.Errorf("invalid git ref %q", newRef)
	}

	if oldRef == "" {
		oldRef, err = getPrevRef(newRef)
		if err != nil {
			return "", "", err
		}
	}
	oldRef = shortenRef(oldRef)
	if ok, err := checkValidRef(oldRef); err != nil {
		return "", "", err
	} else if !ok {
		return "", "", errors.Errorf("invalid git ref %q", oldRef)
	}

	return oldRef, newRef, nil
}

func buildBenches(ctx context.Context, pkgFilter []string, postChck string, bss ...*benchSuite) error {
	// Get the current branch so we can revert to it after, if possible.
	if ref, ok, err := getCurSymbolicRef(); err != nil {
		return err
	} else if ok {
		defer checkoutRef(ref, "")
	}
	now := time.Now() // used to uniquely name artifact files
	for _, bs := range bss {
		if err := bs.build(pkgFilter, postChck, now); err != nil {
			return err
		}
	}
	return nil
}

func runbenchcmpes(ctx context.Context, bs1, bs2 *benchSuite, tests []string, itersPerTest int) error {
	fmt.Println("\nrunning benchmarks:")
	var spinner ui.Spinner
	spinner.Start(os.Stdout, "")
	defer spinner.Stop()
	for i, t := range tests {
		pkg := testBinToPkg(t)
		for j := 0; j < itersPerTest; j++ {
			pkgFrac := ui.Fraction(i+1, len(tests))
			iterFrac := ui.Fraction(j+1, itersPerTest)
			progress := fmt.Sprintf(" pkg=%s iter=%s %s", pkgFrac, iterFrac, pkg)
			spinner.Update(progress)

			// Interleave test suite runs instead of using -count=itersPerTest. The
			// idea is that this reduces the chance that we pick up external noise
			// with a time correlation.
			if err := runSingleBench(bs1, t); err != nil {
				return err
			}
			if err := runSingleBench(bs2, t); err != nil {
				return err
			}
		}
		fmt.Println()
	}
	return nil
}

func runbenchcmp(bs1, bs2 *benchSuite, test string) error {
	// Interleave test suite runs instead of using -count=itersPerTest. The
	// idea is that this reduces the chance that we pick up external noise
	// with a time correlation.
	if err := runSingleBench(bs1, test); err != nil {
		return err
	}
	return runSingleBench(bs2, test)
}

func runSingleBench(bs *benchSuite, test string) error {
	bin := bs.getTestBinary(test)

	// Determine whether the binary has a --logtostderr flag. Use CombinedOutput
	// and ignore the error because --help creates a failed error status. If there
	// is a real error we'll hit it below.
	cmd := exec.Command(bin, "--help")
	out, _ := cmd.CombinedOutput()
	hasLogToStderr := bytes.Contains(out, []byte("logtostderr"))

	// Run the benchmark binary.
	args := []string{bin, "-test.run", "-", "-test.bench", ".", "-test.benchmem"}
	if hasLogToStderr {
		args = append(args, "--logtostderr", "NONE")
	}
	if err := spawnWith(os.Stdin, bs.outFile, bs.outFile, args...); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				// Assume exit code 1 corresponds to a benchmark failure.
				fmt.Println("  saw one or more benchmark failures")
			} else {
				return errors.Wrapf(err, "error running %v: %s", args, exitErr.Stderr)
			}
		} else {
			return errors.Wrapf(err, "error running %v", args)
		}
	}
	return nil
}

func processBenchOutput(ctx context.Context, bs1, bs2 *benchSuite, pkgFilter []string, srv *google.Service) error {
	// We're going to be reading the output files, so seek to the beginning.
	bs1.outFile.Seek(0, io.SeekStart)
	bs2.outFile.Seek(0, io.SeekStart)

	var c benchstat.Collection
	c.Alpha = 0.05
	c.Order = benchstat.Reverse(benchstat.ByDelta) // best first
	c.AddFile("old", bs1.outFile)
	c.AddFile("new", bs2.outFile)
	tables := c.Tables()

	if srv != nil {
		name := fmt.Sprintf("benchcmp: %s (%s -> %s)", strings.Join(pkgFilter, " "), bs1.ref, bs2.ref)
		url, err := srv.CreateSheet(ctx, name, tables)
		if err != nil {
			return err
		}
		fmt.Printf("generated sheet: %s\n", url)
	} else {
		benchstat.FormatText(os.Stdout, tables)
	}
	return nil
}

type benchSuite struct {
	ref       string
	artDir    string
	outFile   *os.File
	binDir    string
	testFiles fileSet
}
type fileSet map[string]struct{}

func makeBenchSuite(ref string) benchSuite {
	return benchSuite{
		ref:       ref,
		testFiles: make(fileSet),
	}
}

func (bs *benchSuite) build(pkgFilter []string, postChck string, t time.Time) (err error) {
	if len(bs.testFiles) != 0 {
		panic("benchSuite already built")
	}

	// Create the artifacts directory: ./benchcmp/<ref>/artifacts
	bs.artDir = testArtifactsDir(bs.ref)
	if err = os.MkdirAll(bs.artDir, 0744); err != nil {
		return err
	}

	// Create output file: ./benchcmp/<ref>/artifacts/out.<time>
	outFileName := bs.getOutputFile(t)
	bs.outFile, err = os.OpenFile(outFileName, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}

	// Create the binary directory: ./benchcmp/<ref>/bin/<hash(pkgFilter)>
	bs.binDir = testBinDir(bs.ref, pkgFilter)
	if _, err = os.Stat(bs.binDir); err == nil {
		fmt.Printf("test binaries already exist for '%s'; skipping build\n", bs.ref)
		files, err := ioutil.ReadDir(bs.binDir)
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.IsDir() {
				return errors.Errorf("unexpected directory %q", f.Name())
			}
			bs.testFiles[f.Name()] = struct{}{}
		}
		return nil
	} else if !os.IsNotExist(err) {
		return errors.Wrap(err, "looking for test directory")
	}
	if err := os.MkdirAll(bs.binDir, 0700); err != nil {
		return err
	}
	// If the binaries are not generated successfully, delete the bin directory
	// so we don't consider the build successful next time benchcmp runs.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(bs.binDir)
		}
	}()

	fmt.Printf("checking out '%s'\n", bs.ref)
	if err := checkoutRef(bs.ref, postChck); err != nil {
		return err
	}

	// Determine which packages to build.
	pkgs, err := expandPackages(pkgFilter)
	if err != nil {
		return err
	}

	var spinner ui.Spinner
	spinner.Start(os.Stdout, fmt.Sprintf("building benchmark binaries for '%s'", bs.ref))
	defer spinner.Stop()
	for i, pkg := range pkgs {
		spinner.Update(ui.Fraction(i, len(pkgs)))
		if testBin, ok, err := buildTestBin(pkg, bs.binDir); err != nil {
			return err
		} else if ok {
			bs.testFiles[testBin] = struct{}{}
		}
	}
	spinner.Update(ui.Fraction(len(pkgs), len(pkgs)))
	return nil
}

func (bs *benchSuite) close() {
	_ = bs.outFile.Close()
}

func (bs *benchSuite) getOutputFile(t time.Time) string {
	const timeFormat = "2006-01-02T15_04_05Z07:00"
	return filepath.Join(bs.artDir, "out."+t.Format(timeFormat))
}

func (bs *benchSuite) getTestBinary(bin string) string {
	return filepath.Join(bs.binDir, bin)
}

func (bs *benchSuite) intersectTests(bs2 *benchSuite) fileSet {
	intersect := make(fileSet)
	for f := range bs.testFiles {
		if _, ok := bs2.testFiles[f]; ok {
			intersect[f] = struct{}{}
		}
	}
	return intersect
}

func (fs fileSet) sorted() []string {
	s := make([]string, 0, len(fs))
	for t := range fs {
		s = append(s, t)
	}
	sort.Strings(s)
	return s
}
