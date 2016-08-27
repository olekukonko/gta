package main

import (
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Masterminds/glide/dependency"
	gpath "github.com/Masterminds/glide/path"
	"github.com/sdboyer/gps"
	"github.com/spf13/cobra"
)

var RootCmd = &cobra.Command{
	Use:   "gta",
	Short: "Ensure that builds work across sets of acceptable dependency versions",
	Long: `gta (gotta test 'em all!') ensures that a build works across ranges of possible
versions for its dependencies.

For example, if your project depends on github.com/foo/bar, and three versions
of that repository exist, then gta can be used to determine if your build will
"work" for each of those versions:

$ gta github.com/foo/bar

By default, gta will simply determine if a dependency solution exists that's
viable for each dep version. However, if a value is passed for --run, then
gta will also execute that command for each solution. ` + "`go test`" + ` is usually
the simplest useful command to run here.

Unless --no-pm is specified, gta will try to detect if metadata files for
package managers (currently only glide) are present. If so, rather than testing
all possible versions of the dependency, it will only check versions that are
allowed by the constraints specified in those files.`,
	RunE: RunGTA,
}

var (
	run                     string
	branch, semver, version string
)

func main() {
	// 1. write basic command, absent manifest/lock loading
	// 2. write support for executing e.g. go test
	// 3. loader for glide files
	RootCmd.Flags().StringVarP(&run, "run", "r", "", "Additional command to run (e.g. `go test`) as a check")
	RootCmd.Flags().StringVarP(&semver, "semver", "v", "", "Semantic version (range or single version) to check")
	RootCmd.Flags().StringVar(&branch, "branch", "", "Branch to check")
	RootCmd.Flags().StringVar(&version, "version", "", "Version (non-semver tag) to check")

	if err := RootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func RunGTA(cmd *cobra.Command, args []string) error {
	var pkg string
	switch len(args) {
	case 1:
		pkg = args[0]
		break
	default:
		return fmt.Errorf("You must specify a single dependency to check against its versions.\n")
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("Could not get working directory: %s", err)
	}

	sm, err := gps.NewSourceManager(dependency.Analyzer{}, filepath.Join(gpath.Home(), "cache"), false)
	defer sm.Release()
	if err != nil {
		return fmt.Errorf("Failed to set up SourceManager: %s", err)
	}

	root, err := sm.DeduceProjectRoot(pkg)
	if err != nil {
		return fmt.Errorf("Could not detect source info for %s: %s", pkg, err)
	}

	vlist, err := sm.ListVersions(root)
	if err != nil {
		return fmt.Errorf("Could not retrieve version list for %s: %s", root, err)
	}

	if len(vlist) == 0 {
		// shouldn't be possible, but whatever
		return fmt.Errorf("No versions could be located for %s", root)
	}

	// obnoxious constraint parsing
	var c gps.Constraint
	switch {
	case branch == "" && semver == "" && version == "":
		c = gps.Any()
	case branch != "":
		if semver != "" || version != "" {
			return fmt.Errorf("Please specify only one type of constraint - branch, version, or semver")
		}
		c = gps.NewBranch(branch)
	case version != "":
		if semver != "" || branch != "" {
			return fmt.Errorf("Please specify only one type of constraint - branch, version, or semver")
		}
		c = gps.NewVersion(version)
	case semver != "":
		if version != "" || branch != "" {
			return fmt.Errorf("Please specify only one type of constraint - branch, version, or semver")
		}
		c, err = gps.NewSemverConstraint(semver)
		if err != nil {
			return fmt.Errorf("%s is not a valid semver constraint", semver)
		}
	}

	// Assume the current directory is correctly placed on a GOPATH, and derive
	// the ProjectRoot from it
	srcprefix := filepath.Join(build.Default.GOPATH, "src") + string(filepath.Separator)
	importroot := filepath.ToSlash(strings.TrimPrefix(wd, srcprefix))

	// Set up params, including tracing
	params := gps.SolveParameters{
		RootDir:    wd,
		ImportRoot: gps.ProjectRoot(importroot),
	}

	var vl []Version
	for _, v := range vlist {
		if c.Matches(v) {
			vl = append(vl, v)
		}
	}

	if len(vl) == 0 {
		return fmt.Errorf("%s has %v versions, but none matched constraint %s", root, len(vlist), c)
	}

	fmt.Println("Checking %s with the following versions: %s", root, vl)

	type solnOrErr struct {
		v   gps.Version
		s   gps.Solution
		err error
	}

	solns := make([]solnOrErr, len(vlist))
	for k, v := range vlist {
		// TODO assign v into manifest
		// TODO parallel, bwahaha
		soe := solnOrErr{v: v}
		// TODO reparse root project every time...horribly wasteful
		s, soe.err = gps.Prepare(params, sm)
		if soe.err == nil {
			soe.s, soe.err = s.Solve()
			continue
		}

		solns[k] = soe
	}

	// If we have to create these vendor trees, then back up the original vendor
	vpath := filepath.Join(root, "vendor")
	if run != "" {
		if _, err = os.Stat(); err != nil {
			err = os.Rename(vpath, filepath.Join(root, "_origvendor"))
			if err != nil {
				return fmt.Errorf("Failed to back up vendor folder: %s", err)
			}
			defer os.Rename(filepath.Join(root, "_origvendor"), vpath)
		}
	}

	var fail bool
	for k, soln := range solns {
		nv := fmt.Sprintf("%s@%s", root, soln.v)
		if soln.err != nil {
			fail = true
			fmt.Printf("%s failed solving: %s\n", nv, soln.err)
			continue
		}

		if run != "" {
			fmt.Printf("%s succeeded", nv)
		} else {
			err = gps.WriteSourceTree(vpath, soln.s, sm)
			if err != nil {
				fail = true
				fmt.Printf("could not write tree for %s, skipping check", nv)
				continue
			}

			parts := strings.Split(run, " ")
			cmd := exec.Command(parts[0], parts[1:]...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				fail = true
				fmt.Printf("%s failed with %s, output:\n%s", err, string(out))
			} else {
				fmt.Printf("%s succeeded", nv)
			}

			os.RemoveAll(vpath)
		}
	}
}
