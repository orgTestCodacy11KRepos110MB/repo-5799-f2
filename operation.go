package f2

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/pterm/pterm"
	"github.com/urfave/cli/v2"

	"github.com/ayoisaiah/f2/internal/utils"
)

const (
	Windows = "windows"
	Darwin  = "darwin"
)

const (
	dotCharacter = 46
)

type renameStatus string

const (
	statusOK                     renameStatus = "ok"
	statusUnchanged              renameStatus = "unchanged"
	statusOverwriting            renameStatus = "overwriting"
	statusEmptyFilename          renameStatus = "empty filename"
	statusTrailingPeriod         renameStatus = "trailing periods are prohibited"
	statusPathExists             renameStatus = "path already exists"
	statusOverwritingNewPath     renameStatus = "overwriting newly renamed path"
	statusInvalidCharacters      renameStatus = "invalid characters present: (%s)"
	statusFilenameLengthExceeded renameStatus = "max file name length exceeded: (%s)"
)

// Change represents a single match in a renaming operation.
type Change struct {
	originalSource string
	status         renameStatus
	BaseDir        string `json:"base_dir"`
	Source         string `json:"source"`
	Target         string `json:"target"`
	Error          string `json:"error,omitempty"`
	csvRow         []string
	index          int
	IsDir          bool `json:"is_dir"`
	WillOverwrite  bool `json:"will_overwrite"`
}

// Operation represents a batch renaming operation.
type Operation struct {
	date               time.Time
	stdin              io.Reader
	stderr             io.Writer
	stdout             io.Writer
	searchRegex        *regexp.Regexp
	conflicts          map[ConflictType][]Conflict
	csvFilename        string
	sort               string
	replacement        string
	workingDir         string
	matches            []Change
	errors             []int
	findSlice          []string
	excludeFilter      []string
	replacementSlice   []string
	pathsToFilesOrDirs []string
	numberOffset       []int
	paths              []Change
	maxDepth           int
	startNumber        int
	replaceLimit       int
	recursive          bool
	ignoreCase         bool
	reverseSort        bool
	onlyDir            bool
	revert             bool
	includeDir         bool
	ignoreExt          bool
	allowOverwrites    bool
	verbose            bool
	includeHidden      bool
	quiet              bool
	fixConflicts       bool
	exec               bool
	stringLiteralMode  bool
	simpleMode         bool
	json               bool
}

type backupFile struct {
	WorkingDir string   `json:"working_dir"`
	Date       string   `json:"date"`
	Operations []Change `json:"operations"`
}

// JSONOutput represents the structure of the output produced by the
// `--json` flag.
type JSONOutput struct {
	Conflicts  map[ConflictType][]Conflict `json:"conflicts,omitempty"`
	WorkingDir string                      `json:"working_dir"`
	Date       string                      `json:"date"`
	Changes    []Change                    `json:"changes"`
	Errors     []int                       `json:"errors,omitempty"`
	DryRun     bool                        `json:"dry_run"`
}

// writeToFile records the details of a successful operation
// to the specified output file, creating it if necessary.
func (op *Operation) writeToFile(outputFile string) (err error) {
	// Create or truncate file
	file, err := os.Create(outputFile)
	if err != nil {
		return err
	}

	defer func() {
		ferr := file.Close()
		if ferr != nil {
			err = ferr
		}
	}()

	mf := backupFile{
		WorkingDir: op.workingDir,
		Date:       time.Now().Format(time.RFC3339),
		Operations: op.matches,
	}

	writer := bufio.NewWriter(file)

	b, err := json.MarshalIndent(mf, "", "    ")
	if err != nil {
		return err
	}

	_, err = writer.Write(b)
	if err != nil {
		return err
	}

	return writer.Flush()
}

// undo reverses a successful renaming operation indicated
// in the specified map file. The undo file is deleted
// if the operation is successfully reverted.
func (op *Operation) undo(path string) error {
	file, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var bf backupFile

	err = json.Unmarshal(file, &bf)
	if err != nil {
		return err
	}

	op.matches = bf.Operations

	for i := range op.matches {
		ch := op.matches[i]

		target := ch.Target
		source := ch.Source

		ch.Source = target
		ch.Target = source

		op.matches[i] = ch
	}

	// Sort only in print mode
	if !op.exec && op.sort != "" {
		err = op.sortBy()
		if err != nil {
			return err
		}
	}

	err = op.apply()
	if err != nil {
		return err
	}

	if op.exec {
		if err = os.Remove(path); err != nil {
			pterm.Fprintln(op.stderr,
				pterm.Warning.Sprintf(
					"Unable to remove redundant backup file '%s' after successful undo operation.",
					pterm.LightYellow(path),
				),
			)
		}
	}

	return nil
}

func (op *Operation) getJSONOutput() ([]byte, error) {
	out := JSONOutput{
		WorkingDir: op.workingDir,
		Date:       op.date.Format(time.RFC3339),
		DryRun:     !op.exec,
		Changes:    op.matches,
		Conflicts:  op.conflicts,
		Errors:     op.errors,
	}

	// prevent empty matches from being encoded as `null`
	if out.Changes == nil {
		out.Changes = make([]Change, 0)
	}

	b, err := json.MarshalIndent(out, "", "    ")
	if err != nil {
		return b, err
	}

	return b, nil
}

// printChanges displays the changes to be made in a
// table or json format.
func (op *Operation) printChanges() {
	if op.quiet {
		return
	}

	if op.json {
		o, err := op.getJSONOutput()
		if err != nil {
			pterm.Fprintln(op.stderr, pterm.Error.Sprint(err))
		}

		pterm.Fprintln(op.stdout, string(o))
	} else {
		data := make([][]string, len(op.matches))

		for i := range op.matches {
			ch := op.matches[i]

			source := filepath.Join(ch.BaseDir, ch.Source)
			target := filepath.Join(ch.BaseDir, ch.Target)

			status := pterm.Green(ch.status)
			if ch.status != statusOK {
				status = pterm.Yellow(ch.status)
			}

			d := []string{source, target, status}
			data[i] = d
		}

		utils.PrintTable(data, op.stdout)
	}
}

// rename iterates over all the matches and renames them on the filesystem
// directories are auto-created if necessary.
// Errors are aggregated instead of being reported one by one.
func (op *Operation) rename() {
	var errs []int

	for i := range op.matches {
		ch := op.matches[i]

		source, target := ch.Source, ch.Target
		source = filepath.Join(ch.BaseDir, source)
		target = filepath.Join(ch.BaseDir, target)

		// skip unchanged file names
		if source == target {
			continue
		}

		// If target contains a slash, create all missing
		// directories before renaming the file
		if strings.Contains(ch.Target, "/") ||
			strings.Contains(ch.Target, `\`) && runtime.GOOS == Windows {
			// No need to check if the `dir` exists or if there are several
			// consecutive slashes since `os.MkdirAll` handles that
			dir := filepath.Dir(ch.Target)

			//nolint:gomnd // number can be understood from context
			err := os.MkdirAll(filepath.Join(ch.BaseDir, dir), 0o750)
			if err != nil {
				errs = append(errs, i)
				op.matches[i].Error = err.Error()

				continue
			}
		}

		if err := os.Rename(source, target); err != nil {
			errs = append(errs, i)
			op.matches[i].Error = err.Error()

			if op.verbose {
				pterm.Fprintln(op.stderr,
					pterm.Error.Sprintf(
						"Failed to rename %s to %s",
						source,
						target,
					),
				)
			}
		} else if op.verbose && !op.json {
			pterm.Success.Printfln("Renamed '%s' to '%s'", pterm.Yellow(source), pterm.Yellow(target))
		}
	}

	op.errors = errs
}

// reportErrors displays the errors that occur during a renaming operation.
func (op *Operation) reportErrors() {
	if op.json {
		o, err := op.getJSONOutput()
		if err != nil {
			pterm.Fprintln(op.stderr, err)
			return
		}

		pterm.Println(string(o))
	} else {
		var data [][]string

		for i := range op.matches {
			ch := op.matches[i]

			if ch.Error != "" {
				continue
			}

			source := filepath.Join(ch.BaseDir, ch.Source)
			target := filepath.Join(ch.BaseDir, ch.Target)
			d := []string{source, target, pterm.Green("success")}

			data = append(data, d)
		}

		for _, num := range op.errors {
			ch := op.matches[num]

			source := filepath.Join(ch.BaseDir, ch.Source)
			target := filepath.Join(ch.BaseDir, ch.Target)

			msg := ch.Error
			if strings.IndexByte(msg, ':') != -1 {
				msg = strings.TrimSpace(msg[strings.IndexByte(msg, ':'):])
			}

			d := []string{
				source,
				target,
				pterm.Red(strings.TrimPrefix(msg, ": ")),
			}
			data = append(data, d)
		}

		utils.PrintTable(data, op.stdout)
	}
}

// handleErrors is used to report errors that occurred while each file was being
// renamed. Successful operations are preserved in a file such that reverting
// the entire process is possible.
func (op *Operation) handleErrors() error {
	op.reportErrors()

	var err error
	if len(op.matches) > len(op.errors) && !op.revert {
		err = op.backup()
	}

	msg := "Some files could not be renamed."

	if op.revert {
		msg = "Some files could not be reverted."
	}

	if err == nil && len(op.matches) > 0 {
		return errors.New(msg)
	} else if err != nil && len(op.matches) > 0 {
		return fmt.Errorf("The above files could not be renamed")
	}

	return fmt.Errorf("The renaming operation failed due to the above errors")
}

// backup creates the path where the backup file
// will be written to.
func (op *Operation) backup() error {
	workingDir := strings.ReplaceAll(op.workingDir, pathSeperator, "_")
	if runtime.GOOS == Windows {
		workingDir = strings.ReplaceAll(workingDir, ":", "_")
	}

	file := workingDir + ".json"

	backupFile, err := xdg.DataFile(filepath.Join("f2", "backups", file))
	if err != nil {
		return err
	}

	return op.writeToFile(backupFile)
}

// noMatches prints out a message if the renaming operation
// failed to match any files.
func (op *Operation) noMatches() {
	msg := "Failed to match any files"
	if op.revert {
		msg = "No operations to undo"
	}

	if op.json {
		b, err := op.getJSONOutput()
		if err != nil {
			pterm.Fprintln(op.stderr, err)
			return
		}

		pterm.Fprintln(op.stdout, string(b))

		return
	}

	pterm.Info.Println(msg)
}

// commit applies the renaming operation to the filesystem.
// A backup file is auto created as long as at least one file
// was renamed and it wasn't an undo operation.
func (op *Operation) commit() error {
	op.rename()

	// print changes in simple mode
	if len(op.errors) == 0 {
		if op.json {
			op.printChanges()
		}
	}

	if len(op.errors) > 0 {
		return op.handleErrors()
	}

	if !op.revert {
		return op.backup()
	}

	return nil
}

// dryRun prints the changes to be made to the standard output.
func (op *Operation) dryRun() {
	op.printChanges()

	if !op.json {
		pterm.Info.Prefix = pterm.Prefix{
			Text:  "DRY RUN",
			Style: pterm.NewStyle(pterm.BgBlue, pterm.FgBlack),
		}

		pterm.Fprintln(
			op.stdout,
			pterm.Info.Sprint(
				"Commit the above changes with the -x/--exec flag",
			),
		)
	}
}

// apply prints the changes to be made in dry-run mode
// or commits the operation to the filesystem if in execute mode.
// If conflicts are detected, the operation is aborted and the conflicts
// are printed out so that they may be corrected by the user.
func (op *Operation) apply() error {
	if len(op.matches) == 0 {
		op.noMatches()
		return nil
	}

	op.detectConflicts()

	if len(op.conflicts) > 0 && !op.fixConflicts {
		if op.json {
			op.printChanges()
		} else {
			op.reportConflicts()
		}

		return errConflictDetected
	}

	if op.includeDir || op.revert {
		op.sortMatches()
	}

	if op.exec {
		if op.simpleMode && !op.json {
			op.printChanges()

			reader := bufio.NewReader(os.Stdin)

			fmt.Print("\033[s")
			fmt.Print("Press ENTER to commit the above changes")

			// Block until user input before beginning next session
			_, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		}

		return op.commit()
	}

	op.dryRun()

	return nil
}

// findMatches locates matches for the search pattern
// in each filename. Hidden files and directories are exempted
// by default.
func (op *Operation) findMatches() error {
	for i := range op.paths {
		ch := op.paths[i]

		filename := filepath.Base(ch.Source)

		if ch.IsDir && !op.includeDir {
			continue
		}

		if op.onlyDir && !ch.IsDir {
			continue
		}

		// ignore dotfiles on unix and hidden files on windows
		if !op.includeHidden {
			hidden, err := isHidden(filename, ch.BaseDir)
			if err != nil {
				return err
			}

			if hidden {
				absPath1, err := filepath.Abs(
					filepath.Join(ch.BaseDir, filename),
				)
				if err != nil {
					return err
				}

				shouldSkip := true

				for _, path := range op.pathsToFilesOrDirs {
					absPath2, err := filepath.Abs(path)
					if err != nil {
						return err
					}

					if strings.EqualFold(absPath1, absPath2) {
						shouldSkip = false
					}
				}

				if shouldSkip {
					continue
				}
			}
		}

		f := filename
		if op.ignoreExt && !ch.IsDir {
			f = utils.FilenameWithoutExtension(f)
		}

		matched := op.searchRegex.MatchString(f)
		if matched {
			op.matches = append(op.matches, ch)
		}
	}

	return nil
}

// filterMatches excludes any files or directories that match
// the find pattern in accordance with the provided exclude pattern.
func (op *Operation) filterMatches() error {
	var filtered []Change

	filters := strings.Join(op.excludeFilter, "|")

	regex, err := regexp.Compile(filters)
	if err != nil {
		return err
	}

	for i := range op.matches {
		ch := op.matches[i]

		if !regex.MatchString(ch.Source) {
			filtered = append(filtered, ch)
		}
	}

	op.matches = filtered

	return nil
}

// setPaths creates a Change struct for each path.
func (op *Operation) setPaths(paths map[string][]os.DirEntry) {
	if op.exec {
		if !indexVarRegex.MatchString(op.replacement) {
			op.paths = op.sortPaths(paths, false)
			return
		}
	}

	// Don't bother sorting the paths in alphabetical order
	// if a different sort has been set that's not the default
	if op.sort != "" && op.sort != "default" {
		op.paths = op.sortPaths(paths, false)
		return
	}

	op.paths = op.sortPaths(paths, true)
}

// retrieveBackupFile retrieves the path to a previously created
// backup file for the current directory.
func (op *Operation) retrieveBackupFile() (string, error) {
	dir := strings.ReplaceAll(op.workingDir, pathSeperator, "_")
	if runtime.GOOS == Windows {
		dir = strings.ReplaceAll(dir, ":", "_")
	}

	file := dir + ".json"

	fullPath, err := xdg.SearchDataFile(filepath.Join("f2", "backups", file))
	if err != nil {
		return "", err
	}

	return fullPath, nil
}

// handleReplacementChain is ensures that each find
// and replace operation (single or chained) is handled correctly.
func (op *Operation) handleReplacementChain() error {
	for i, v := range op.replacementSlice {
		op.replacement = v

		err := op.replace()
		if err != nil {
			return err
		}

		for j := range op.matches {
			ch := op.matches[j]

			// Update the source to the target from the previous replacement
			// in preparation for the next replacement
			if i != len(op.replacementSlice)-1 {
				op.matches[j].Source = ch.Target
			}

			// After the last replacement, update the Source
			// back to the original
			if i > 0 && i == len(op.replacementSlice)-1 {
				op.matches[j].Source = ch.originalSource
			}
		}

		if i != len(op.replacementSlice)-1 {
			err := op.setFindStringRegex(i + 1)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// run executes the operation sequence.
func (op *Operation) run() error {
	if op.revert {
		path, err := op.retrieveBackupFile()
		if err != nil {
			return errBackupNotFound
		}

		return op.undo(path)
	}

	err := op.findMatches()
	if err != nil {
		return err
	}

	if len(op.excludeFilter) != 0 {
		err = op.filterMatches()
		if err != nil {
			return err
		}
	}

	if op.sort != "" {
		err = op.sortBy()
		if err != nil {
			return err
		}
	}

	err = op.handleReplacementChain()
	if err != nil {
		return err
	}

	return op.apply()
}

// setFindStringRegex compiles a regular expression for the
// find string of the corresponding replacement index (if any).
// Otherwise, the created regex will match the entire file name.
func (op *Operation) setFindStringRegex(replacementIndex int) error {
	// findPattern is set to match the entire file name by default
	// except if a find string for the corresponding replacement index
	// is found
	findPattern := ".*"
	if len(op.findSlice) > replacementIndex {
		findPattern = op.findSlice[replacementIndex]

		// Escape all regular expression metacharacters in string literal mode
		if op.stringLiteralMode {
			findPattern = regexp.QuoteMeta(findPattern)
		}

		if op.ignoreCase {
			findPattern = "(?i)" + findPattern
		}
	}

	re, err := regexp.Compile(findPattern)
	if err != nil {
		return err
	}

	op.searchRegex = re

	return nil
}

func removeHidden(
	de []os.DirEntry,
	baseDir string,
) (ret []os.DirEntry, err error) {
	for _, e := range de {
		r, err := isHidden(e.Name(), baseDir)
		if err != nil {
			return nil, err
		}

		if !r {
			ret = append(ret, e)
		}
	}

	return ret, nil
}

// walk is used to navigate directories recursively
// and include their contents in the pool of paths in
// which to find matches. It respects the following properties
// set on the operation: whether hidden files should be
// included, and the maximum depth limit (0 for no limit).
// The paths argument is modified in place.
func (op *Operation) walk(paths map[string][]os.DirEntry) error {
	var recursedPaths []string

	var currentDepth int

	// currentLevel represents the current level of directories
	// and their contents
	currentLevel := make(map[string][]os.DirEntry)

loop:
	// The goal of each iteration is to created entries for each
	// unaccounted directory in the current level
	for dir, dirContents := range paths {
		if utils.Contains(recursedPaths, dir) {
			continue
		}

		if !op.includeHidden {
			var err error
			dirContents, err = removeHidden(dirContents, dir)
			if err != nil {
				return err
			}
		}

		for _, entry := range dirContents {
			if entry.IsDir() {
				fp := filepath.Join(dir, entry.Name())
				dirEntry, err := os.ReadDir(fp)
				if err != nil {
					return err
				}

				currentLevel[fp] = dirEntry
			}
		}

		recursedPaths = append(recursedPaths, dir)
	}

	// if there are directories in the current level
	// store each directory entry and empty the
	// currentLevel so that it may be repopulated
	if len(currentLevel) > 0 {
		for dir, dirContents := range currentLevel {
			paths[dir] = dirContents

			delete(currentLevel, dir)
		}

		currentDepth++
		if !(op.maxDepth > 0 && currentDepth == op.maxDepth) {
			goto loop
		}
	}

	return nil
}

// handleCSV reads the provided CSV file, and finds all the
// valid candidates for replacement.
func (op *Operation) handleCSV(paths map[string][]fs.DirEntry) error {
	records, err := utils.ReadCSVFile(op.csvFilename)
	if err != nil {
		return err
	}

	var csvPaths []Change

	for i, record := range records {
		if len(record) == 0 {
			continue
		}

		source := strings.TrimSpace(record[0])

		var targetName string

		var found bool

		if len(record) > 1 {
			targetName = strings.TrimSpace(record[1])
		}

		pathMap := make(map[string]os.FileInfo)

		for k := range paths {
			fullPath := source

			if !filepath.IsAbs(source) {
				fullPath = filepath.Join(k, source)
			}

			if f, err := os.Stat(fullPath); err == nil ||
				errors.Is(err, os.ErrExist) {
				pathMap[fullPath] = f
				found = true
			}
		}

		if !found && op.verbose {
			pterm.Fprintln(op.stderr,
				pterm.Warning.Sprintf(
					"Source file '%s' was not found, so row '%d' was skipped",
					source,
					i+1,
				),
			)
		}

	loop:
		for path, fileInfo := range pathMap {
			dir := filepath.Dir(path)

			vars, err := extractVariables(targetName)
			if err != nil {
				return err
			}

			ch := Change{
				BaseDir:        dir,
				Source:         filepath.Clean(fileInfo.Name()),
				originalSource: filepath.Clean(fileInfo.Name()),
				csvRow:         record,
				IsDir:          fileInfo.IsDir(),
				Target:         targetName,
			}

			err = op.replaceVariables(&ch, &vars)
			if err != nil {
				return err
			}

			// ensure the same the same path is not added more than once
			for i := range csvPaths {
				v1 := csvPaths[i]

				fullPath := filepath.Join(v1.BaseDir, v1.Source)
				if fullPath == path {
					break loop
				}
			}

			csvPaths = append(csvPaths, ch)
		}
	}

	op.paths = csvPaths

	return nil
}

// setDefaultOpts applies the options that may be set through
// F2_DEFAULT_OPTS.
func setDefaultOpts(op *Operation, c *cli.Context) {
	op.fixConflicts = c.Bool("fix-conflicts")
	op.includeDir = c.Bool("include-dir")
	op.includeHidden = c.Bool("hidden")
	op.ignoreCase = c.Bool("ignore-case")
	op.ignoreExt = c.Bool("ignore-ext")
	op.recursive = c.Bool("recursive")
	op.onlyDir = c.Bool("only-dir")
	op.stringLiteralMode = c.Bool("string-mode")
	op.excludeFilter = c.StringSlice("exclude")
	op.maxDepth = int(c.Uint("max-depth"))
	op.verbose = c.Bool("verbose")
	op.allowOverwrites = c.Bool("allow-overwrites")
	op.replaceLimit = c.Int("replace-limit")
	op.quiet = c.Bool("quiet")
	op.json = c.Bool("json")

	// Sorting
	if c.String("sort") != "" {
		op.sort = c.String("sort")
	} else if c.String("sortr") != "" {
		op.sort = c.String("sortr")
		op.reverseSort = true
	}

	if op.onlyDir {
		op.includeDir = true
	}
}

// setOptions sets the options on the operation based on
// command-line arguments.
func setOptions(op *Operation, c *cli.Context) error {
	if len(c.StringSlice("find")) == 0 &&
		len(c.StringSlice("replace")) == 0 &&
		c.String("csv") == "" &&
		!c.Bool("undo") {
		return errInvalidArgument
	}

	op.findSlice = c.StringSlice("find")
	op.replacementSlice = c.StringSlice("replace")
	op.csvFilename = c.String("csv")
	op.revert = c.Bool("undo")
	op.pathsToFilesOrDirs = c.Args().Slice()
	op.exec = c.Bool("exec")

	setDefaultOpts(op, c)

	// Ensure that each findString has a corresponding replacement.
	// The replacement defaults to an empty string if unset
	for len(op.findSlice) > len(op.replacementSlice) {
		op.replacementSlice = append(op.replacementSlice, "")
	}

	return op.setFindStringRegex(0)
}

// setSimpleModeOptions is used to set the options for the
// renaming operation in simpleMode.
func setSimpleModeOptions(op *Operation, c *cli.Context) error {
	args := c.Args().Slice()

	if len(args) < 1 {
		return errInvalidSimpleModeArgs
	}

	// If a replacement string is not specified, it shoud be
	// an empty string
	if len(args) == 1 {
		args = append(args, "")
	}

	minArgs := 2

	op.simpleMode = true
	op.exec = true

	op.findSlice = []string{args[0]}
	op.replacementSlice = []string{args[1]}

	setDefaultOpts(op, c)

	op.includeDir = true

	if len(args) > minArgs {
		op.pathsToFilesOrDirs = args[minArgs:]
	}

	return op.setFindStringRegex(0)
}

// newOperation returns an Operation constructed
// from command line flags & arguments.
func newOperation(c *cli.Context) (*Operation, error) {
	op := &Operation{
		stdout: os.Stdout,
		stderr: os.Stderr,
		stdin:  os.Stdin,
		date:   time.Now(),
	}

	v, exists := c.App.Metadata["reader"]
	if exists {
		r, ok := v.(io.Reader)
		if ok {
			op.stdin = r
		}
	}

	v, exists = c.App.Metadata["writer"]
	if exists {
		w, ok := v.(io.Writer)
		if ok {
			op.stdout = w
		}
	}

	var err error

	if _, ok := c.App.Metadata["simple-mode"]; ok {
		err = setSimpleModeOptions(op, c)
		if err != nil {
			return nil, err
		}
	} else {
		err = setOptions(op, c)
		if err != nil {
			return nil, err
		}
	}

	// Get the current working directory
	op.workingDir, err = filepath.Abs(".")
	if err != nil {
		return nil, err
	}

	// If reverting an operation, no need to walk through directories
	if op.revert {
		return op, nil
	}

	paths := make(map[string][]os.DirEntry)

	for _, path := range op.pathsToFilesOrDirs {
		var fileInfo os.FileInfo

		path = filepath.Clean(path)

		// Skip paths that have already been processed
		if _, ok := paths[path]; ok {
			continue
		}

		fileInfo, err = os.Stat(path)
		if err != nil {
			return nil, err
		}

		if fileInfo.IsDir() {
			paths[path], err = os.ReadDir(path)
			if err != nil {
				return nil, err
			}

			continue
		}

		dir := filepath.Dir(path)

		var dirEntry []fs.DirEntry

		dirEntry, err = os.ReadDir(dir)
		if err != nil {
			return nil, err
		}

	entryLoop:
		for _, entry := range dirEntry {
			if entry.Name() == fileInfo.Name() {
				// Ensure that the file is not already
				// present in the directory entry
				for _, e := range paths[dir] {
					if e.Name() == fileInfo.Name() {
						break entryLoop
					}
				}

				paths[dir] = append(paths[dir], entry)

				break
			}
		}
	}

	// Use current directory
	if len(paths) == 0 {
		paths["."], err = os.ReadDir(".")
		if err != nil {
			return nil, err
		}
	}

	if op.recursive {
		err = op.walk(paths)
		if err != nil {
			return nil, err
		}
	}

	op.setPaths(paths)

	if op.csvFilename != "" {
		err = op.handleCSV(paths)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", errCSVReadFailed, err.Error())
		}
	}

	return op, nil
}
