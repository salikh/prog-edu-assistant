// Package autograder provides the logic to parse the Jupyter notebook submissions,
// extract the assignment ID, match the assignment to the autograder scripts,
// set up the scratch directory and run the autograder tests under nsjail.
package autograder

import (
	"bytes"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"text/template"

	"github.com/golang/glog"
	"github.com/google/prog-edu-assistant/notebook"
)

// Autograder encapsulates the setup of autograder scripts.
type Autograder struct {
	// Dir points to the root directory of autograder scripts.
	// Under Dir, the first level directory names are matched to assignment_id,
	// second level to exercise_id. In the second-level directories,
	// python unit test files (*Test.py) should be present.
	Dir string
	// ScratchDir points to the directory where one can write, /tmp by default.
	ScratchDir string
	// NSJailPath is the path to nsjail, /usr/local/bin/nsjail by default.
	NSJailPath string
	// PythonPath is the path to python binary, /usr/bin/python by default.
	PythonPath string
	// DisableCleanup instructs the autograder not to delete the scratch directory.
	DisableCleanup bool
}

// New creates a new autograder instance given the autograder directory.
func New(dir string) *Autograder {
	return &Autograder{
		Dir:        dir,
		ScratchDir: "/tmp",
		NSJailPath: "/usr/local/bin/nsjail",
		PythonPath: "/usr/bin/python",
	}
}

type InlineTestFill struct {
	Context    string
	Submission string
	Inline     string
}

// The output format uses double braces to facilitate parsing
// of the output by regexps.
var inlineTestTmpl = template.Must(template.New("inlinetest").Parse(`
try:
  {{.Context}}
except Exception as e:
  print("\nWhile executing context: ERROR{{"{{"}}%s{{"}}"}}" % e)
try:
  {{.Submission}}
except Exception as e:
  print("\nWhile executing submission: ERROR{{"{{"}}%s{{"}}"}}" % e)
try:
  {{.Inline}}
  print("OK{{"{{}}"}}")
except AssertionError as e:
  print("\nWhile executing inline test: FAIL{{"{{"}}%s{{"}}"}}" % str(e))
except Exception as e:
  print("\nWhile executing inline test: ERROR{{"{{"}}%s{{"}}"}}" % e)
`))

func generateInlineTest(context, submission, test string) ([]byte, error) {
	var output bytes.Buffer
	err := inlineTestTmpl.Execute(&output, &InlineTestFill{
		// Indent the parts by two spaces to match the template.
		Context:    strings.ReplaceAll(context, "\n", "\n  "),
		Submission: strings.ReplaceAll(submission, "\n", "\n  "),
		Inline:     strings.ReplaceAll(test, "\n", "\n  "),
	})
	if err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// CreateScratchDir takes the submitted contents of a solution cell,
// the source exercise directory and sets up the scratch directory
// for autograding.
func (ag *Autograder) CreateScratchDir(exerciseDir, scratchDir string, submission []byte) error {
	err := CopyDirFiles(exerciseDir, scratchDir)
	if err != nil {
		return fmt.Errorf("error copying autograder scripts from %q to %q: %s", exerciseDir, scratchDir, err)
	}
	// TODO(salikh): Implement proper scratch management with overlayfs.
	filename := filepath.Join(scratchDir, "submission.py")
	err = ioutil.WriteFile(filename, submission, 0775)
	if err != nil {
		return fmt.Errorf("error writing to %q: %s", filename, err)
	}
	filename = filepath.Join(scratchDir, "submission_source.py")
	content := bytes.Join([][]byte{[]byte(`source = """`),
		bytes.ReplaceAll(submission, []byte(`"""`), []byte(`\"\"\"`)), []byte(`"""`)}, nil)
	err = ioutil.WriteFile(filename, content, 0775)
	if err != nil {
		return fmt.Errorf("error writing to %q: %s", filename, err)
	}
	// Synthesize the inline tests.
	pattern := filepath.Join(exerciseDir, "*_inline.py")
	inlinetests, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("error in filepath.Glob(%q): %s", pattern, err)
	}
	for _, inlineTestFilename := range inlinetests {
		contextFilename := strings.ReplaceAll(inlineTestFilename, "_inline.py", "_context.py")
		contextContent, err := ioutil.ReadFile(contextFilename)
		if err != nil {
			return fmt.Errorf("error reading context file %q: %s", contextFilename, err)
		}
		testContent, err := ioutil.ReadFile(inlineTestFilename)
		if err != nil {
			return fmt.Errorf("error reading inline test file %q: %s", inlineTestFilename, err)
		}
		output, err := generateInlineTest(string(contextContent), string(submission), string(testContent))
		if err != nil {
			return fmt.Errorf("error generating inline test from template: %s", err)
		}
		outputFilename := filepath.Join(scratchDir,
			strings.ReplaceAll(filepath.Base(inlineTestFilename), "_inline.py", "_inlinetest.py"))
		err = ioutil.WriteFile(outputFilename, output, 0775)
		if err != nil {
			return fmt.Errorf("error writing the inline test file %q: %s", outputFilename, err)
		}
	}
	return nil
}

func joinInlineReports(inlineReports map[string]string) string {
	var parts []string
	for name, report := range inlineReports {
		parts = append(parts, "<h4 style='color: #387;'>"+name+"</h4>")
		parts = append(parts, report)
	}
	return strings.Join(parts, "\n")
}

// Grade takes a byte blob, tries to parse it as JSON, then tries to extract
// the metadata and match it to the available corpus of autograder scripts.
// If found, it then proceeds to run all autograder scripts under nsjail,
// parse the output, and produce the report, also in JSON format.
func (ag *Autograder) Grade(notebookBytes []byte) ([]byte, error) {
	data := make(map[string]interface{})
	err := json.Unmarshal(notebookBytes, &data)
	if err != nil {
		return nil, fmt.Errorf("could not parse request as JSON: %s", err)
	}
	v, ok := data["metadata"]
	if !ok {
		return nil, fmt.Errorf("request did not have .metadata")
	}
	metadata, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("metadata is not a map, but %s", reflect.TypeOf(v))
	}
	v, ok = metadata["submission_id"]
	if !ok {
		return nil, fmt.Errorf("request did not have submission_id")
	}
	submissionID, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("metadata.submission_id is not a string but %s",
			reflect.TypeOf(v))
	}
	v, ok = metadata["assignment_id"]
	if !ok {
		return nil, fmt.Errorf("metadata does not have assignment_id")
	}
	assignmentID, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("metadata.assignment_id is not a string but %s",
			reflect.TypeOf(v))
	}
	dir := filepath.Join(ag.Dir, assignmentID)
	glog.V(3).Infof("assignment dir: %s", dir)
	fs, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("assignment dir %q with id %q does not exit: %s", dir, assignmentID, err)
	}
	if !fs.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", dir)
	}
	n, err := notebook.Parse(notebookBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing the submitted blob as Jupyter notebook: %s", err)
	}
	allOutcomes := make(map[string]bool)
	allReports := make(map[string]string)
	allLogs := make(map[string]interface{})
	baseScratchDir := filepath.Join(ag.ScratchDir, submissionID)
	err = os.MkdirAll(baseScratchDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("error making dir %q: %s", baseScratchDir, err)
	}
	if !ag.DisableCleanup {
		defer func() {
			_ = os.RemoveAll(baseScratchDir)
		}()
	}
	for _, cell := range n.Cells {
		if cell.Metadata == nil {
			continue
		}
		v, ok := cell.Metadata["exercise_id"]
		if !ok {
			// Skip all non-solution cells.
			continue
		}
		exerciseID, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("exercise_id is not a string but %s",
				reflect.TypeOf(v))
		}
		exerciseDir := filepath.Join(dir, exerciseID)
		fs, err = os.Stat(exerciseDir)
		if err != nil {
			return nil, fmt.Errorf("exercise with id %s/%s does not exit",
				assignmentID, exerciseID)
		}
		if !fs.IsDir() {
			return nil, fmt.Errorf("%q is not a directory", exerciseDir)
		}
		scratchDir := filepath.Join(baseScratchDir, exerciseID)
		glog.Infof("scratch dir: %s", scratchDir)
		err := ag.CreateScratchDir(exerciseDir, scratchDir, []byte(cell.Source))
		if err != nil {
			return nil, fmt.Errorf("error creating scratch dir %s: %s", scratchDir, err)
		}
		glog.V(3).Infof("Running tests in directory %s", scratchDir)
		outcomes, logs, err := ag.RunUnitTests(scratchDir)
		if err != nil {
			return nil, fmt.Errorf("error running unit tests in %q: %s", scratchDir, err)
		}
		inlineOutcomes, inlineLogs, inlineReports, err := ag.RunInlineTests(scratchDir)
		if err != nil {
			return nil, fmt.Errorf("error running inline tests in %q: %s", scratchDir, err)
		}
		mergedOutcomes := make(map[string]interface{})
		mergedLogs := make(map[string]string)
		for k, v := range outcomes {
			mergedOutcomes[k] = v
		}
		for k, v := range inlineOutcomes {
			mergedOutcomes[k] = v
		}
		for k, v := range logs {
			mergedLogs[k] = v
		}
		for k, v := range inlineLogs {
			mergedLogs[k] = v
		}
		// Small data for the report generation.
		outcomeData := map[string]interface{}{
			"results": mergedOutcomes,
			"logs":    mergedLogs,
		}
		report, err := ag.RenderReports(scratchDir, outcomeData)
		if err != nil {
			return nil, err
		}
		allReports[exerciseID] = string(report) + joinInlineReports(inlineReports)
		allLogs[exerciseID] = logs
		for k, v := range outcomes {
			_, ok := allOutcomes[k]
			if ok {
				return nil, fmt.Errorf("duplicated unit test %q", k)
			}
			allOutcomes[k] = v
		}
	}
	result := make(map[string]interface{})
	result["assignment_id"] = assignmentID
	result["submission_id"] = submissionID
	result["outcomes"] = allOutcomes
	result["logs"] = allLogs
	result["reports"] = allReports
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("error serializing report json: %s", err)
	}
	return b, nil
}

var outcomeRegex = regexp.MustCompile(`(test[a-zA-Z0-9_]*) \(([a-zA-Z0-9_-]+)\.([a-zA-Z0-9_]*)\) \.\.\. (ok|FAIL|ERROR)`)

// RunUnitTests runs all tests in a scratch directory found by a glob *Test.py.
func (ag *Autograder) RunUnitTests(dir string) (map[string]bool, map[string]string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting abs path for %q: %s", dir, err)
	}
	err = os.Chdir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("error on chdir %q: %s", dir, err)
	}
	fss, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("error on listing %q: %s", dir, err)
	}
	outcomes := make(map[string]bool)
	logs := make(map[string]string)
	for _, fs := range fss {
		filename := fs.Name()
		if !strings.HasSuffix(filename, "Test.py") {
			continue
		}
		// nsjail -Mo --time_limit 2 --max_cpus 1 --rlimit_as 700 -E LANG=en_US.UTF-8 --disable_proc --chroot / --cwd $PWD --user nobody --group nogroup --iface_no_lo -- /usr/bin/python3 -m unittest discover -v -p '*Test.py'
		cmd := exec.Command(ag.NSJailPath,
			"-Mo",
			// NSJail does not work under docker without these disable flags.
			"--disable_clone_newcgroup",
			"--disable_clone_newipc",
			"--disable_clone_newnet",
			"--disable_clone_newns",
			"--disable_clone_newpid",
			"--disable_clone_newuser",
			"--disable_clone_newuts",
			"--disable_no_new_privs",
			"--time_limit", "3",
			"--max_cpus", "1",
			"--rlimit_as", "700",
			"--env", "LANG=en_US.UTF-8",
			"--disable_proc",
			//"--chroot", "/",
			"--cwd", dir,
			"--user", "nobody",
			"--group", "nogroup",
			"--iface_no_lo",
			"--",
			ag.PythonPath, "-m", "unittest",
			"-v", fs.Name())
		glog.V(5).Infof("about to execute %s %q", cmd.Path, cmd.Args)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				return nil, nil, fmt.Errorf("error running unit test command %q %q: %s", cmd.Path, cmd.Args, err)
			}
			// Overall status was non-ok.
			outcomes[filename] = false
		} else {
			// The file run okay.
			outcomes[filename] = true
		}
		logs[filename] = string(out)
		// TODO(salikh): Implement a more robust way of reporting individual
		// test statuses from inside the test runner.
		mm := outcomeRegex.FindAllSubmatch(out, -1)
		if len(mm) == 0 {
			// Cannot find any individual test case outcomes.
			outcomes[filename] = false
			continue
		}
		for _, m := range mm {
			method := string(m[1])
			className := string(m[3])
			status := string(m[4])
			key := className + "." + method
			if status == "ok" {
				outcomes[key] = true
			} else {
				outcomes[key] = false
			}
		}
	}
	return outcomes, logs, nil
}

var inlineOutcomeRegex = regexp.MustCompile(`(OK|ERROR|FAIL){{((?:[^}]|}[^}])*)}}`)

type inlineReportFill struct {
	Passed bool
	Error  string
}

var inlineReportTmpl = htmltemplate.Must(htmltemplate.New("inlinereport").Parse(
	`{{if .Passed}}
Looks OK.
{{else}}
{{.Error}}
{{end}}
`))

// RunInlineTest runs the inline test specified by the filename (with
// user-sumitted code already written into the file surrounded by the context
// code and test code appropriately). It assumes the filename has the form of
// TestName_inlinetest.py.
// Returns the outcome JSON object with the following fields:
// * logs: the output of the test run, useful for debugging.
// * outcomes: the dictionary with the following fields:
//   - run: boolean indicating whether the test was run.
//   - passed: boolean indicating whether the test run passed.
//   - error: a string with an error message (when the test failed).
// * report: a short pre-rendered autogenerated report for inline test.
func (ag *Autograder) RunInlineTest(filename string) (map[string]interface{}, error) {
	return nil, fmt.Errorf("TODO(salikh): Implement RunInlineTest.")
}

// RunInlineTests runs all inline tests in a scratch directory found by a glob
// *_inlinetest.py.
// Returns
// - outcomes map[string]interface{}
// - logs map[string]string
// - reports map[string]string
func (ag *Autograder) RunInlineTests(dir string) (map[string]interface{}, map[string]string, map[string]string, error) {
	glog.V(3).Infof("RunInlineTests(%s)", dir)
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error getting abs path for %q: %s", dir, err)
	}
	err = os.Chdir(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error on chdir %q: %s", dir, err)
	}
	fss, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error on listing %q: %s", dir, err)
	}
	outcomes := make(map[string]interface{})
	reports := make(map[string]string)
	logs := make(map[string]string)
	for _, fs := range fss {
		filename := fs.Name()
		if !strings.HasSuffix(filename, "_inlinetest.py") {
			continue
		}
		fileOutcome := make(map[string]interface{})
		outcomes[filename] = fileOutcome
		cmd := exec.Command(ag.NSJailPath,
			"-Mo",
			// NSJail does not work under docker without these disable flags.
			"--disable_clone_newcgroup",
			"--disable_clone_newipc",
			"--disable_clone_newnet",
			"--disable_clone_newns",
			"--disable_clone_newpid",
			"--disable_clone_newuser",
			"--disable_clone_newuts",
			"--disable_no_new_privs",
			"--time_limit", "3",
			"--max_cpus", "1",
			"--rlimit_as", "700",
			"--env", "LANG=en_US.UTF-8",
			"--disable_proc",
			//"--chroot", "/",
			"--cwd", dir,
			"--user", "nobody",
			"--group", "nogroup",
			"--iface_no_lo",
			"--",
			ag.PythonPath,
			fs.Name())
		glog.V(5).Infof("about to execute %s %q", cmd.Path, cmd.Args)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				return nil, nil, nil, fmt.Errorf("error running unit test command %q %q: %s", cmd.Path, cmd.Args, err)
			}
			// Overall status was non-ok.
			fileOutcome["run"] = false
		} else {
			// The file was run successfully.
			fileOutcome["run"] = true
		}
		logs[filename] = string(out)
		mm := inlineOutcomeRegex.FindAllSubmatch(out, -1)
		if len(mm) == 0 {
			// Cannot find any individual test case outcomes.
			fileOutcome["passed"] = false
			continue
		}
		var reportBuf bytes.Buffer
		for _, m := range mm {
			status := string(m[0])
			message := string(m[1])
			if status == "OK" {
				if _, ok := fileOutcome["passed"]; !ok {
					fileOutcome["passed"] = true
				}
			} else {
				fileOutcome["passed"] = false
			}
			if status == "ERROR" {
				message = "Internal test error: " + message
			}
			if message != "" {
				if old, ok := fileOutcome["error"]; ok {
					fileOutcome["error"] = old.(string) + "\n" + message
				} else {
					fileOutcome["error"] = message
				}
			}
			err := inlineReportTmpl.Execute(&reportBuf, &inlineReportFill{
				Passed: status == "OK",
				Error:  message,
			})
			if err != nil {
				return nil, nil, nil, err
			}
		}
		reports[filename] = reportBuf.String()
	}
	return outcomes, logs, reports, nil
}

// RenderReports looks for report templates in the specified scratch dir and renders all reports.
// It returns the concatenation of all reports output.
func (ag *Autograder) RenderReports(dir string, data map[string]interface{}) ([]byte, error) {
	err := os.Chdir(dir)
	if err != nil {
		return nil, fmt.Errorf("error on chdir %q: %s", dir, err)
	}
	fss, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("error on listing %q: %s", dir, err)
	}
	dataJson, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var reports [][]byte
	for _, fs := range fss {
		filename := fs.Name()
		if !strings.HasSuffix(filename, "_template.py") {
			continue
		}
		cmd := exec.Command("python", filename)
		glog.V(3).Infof("Starting command %s %q with input %q", cmd.Path, cmd.Args, string(dataJson))
		cmdIn, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		go func() {
			cmdIn.Write(dataJson)
			cmdIn.Close()
		}()
		output, err := cmd.CombinedOutput()
		if err != nil {
			reports = append(reports, []byte(fmt.Sprintf(`
<h2 style='color: red'>Reporter error</h2>
<pre>%s</pre>`, err.Error())))
			glog.Errorf("Reporter error: %s", err)
			reports = append(reports, output)
			continue
		}
		glog.V(3).Infof("Output: %s", string(output))
		reports = append(reports, output)
	}
	return bytes.Join(reports, nil), nil
}

// CopyDirFiles copies all files in the directory (one level).
func CopyDirFiles(src, dest string) error {
	err := os.MkdirAll(dest, 0755)
	if err != nil {
		return fmt.Errorf("error creating dir %q: %s", dest, err)
	}
	fss, err := ioutil.ReadDir(src)
	if err != nil {
		return fmt.Errorf("error listing dir %q: %s", src, err)
	}
	for _, fs := range fss {
		if fs.IsDir() {
			return fmt.Errorf(" CopyDirFiles: copying dirs recursively not implemented (%s/%s)", src, fs.Name())
		}
		filename := filepath.Join(src, fs.Name())
		b, err := ioutil.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("error reading %q: %s", filename, err)
		}
		filename = filepath.Join(dest, fs.Name())
		err = ioutil.WriteFile(filename, b, 0644)
		if err != nil {
			return fmt.Errorf("error writing %q: %s", filename, err)
		}
		glog.V(5).Infof("copied %s from %s to %s", fs.Name(), src, dest)
	}
	return nil
}
