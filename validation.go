package f2

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/pterm/pterm"

	"github.com/ayoisaiah/f2/internal/utils"
)

var (
	// partialWindowsForbiddenCharRegex is used to match the strings that contain forbidden
	// characters in Windows' file names. This does not include also forbidden
	// forward and back slash characters because their presence will cause a new
	// directory to be created.
	partialWindowsForbiddenCharRegex = regexp.MustCompile(`<|>|:|"|\||\?|\*`)
	// completeWindowsForbiddenCharRegex is like windowsForbiddenRegex but includes
	// forward and backslashes.
	completeWindowsForbiddenCharRegex = regexp.MustCompile(
		`<|>|:|"|\||\?|\*|/|\\`,
	)
	// macForbiddenCharRegex is used to match the strings that contain forbidden
	// characters in macOS' file names.
	macForbiddenCharRegex = regexp.MustCompile(`:`)
)

const (
	// max filename length of 260 characters in Windows.
	windowsMaxLength = 260
	// max filename length of 255 bytes on Linux and other unix-based OSes.
	unixMaxBytes = 255
)

type ConflictType string

const (
	emptyFilename             ConflictType = "emptyFilename"
	fileExists                ConflictType = "fileExists"
	overwritingNewPath        ConflictType = "overwritingNewPath"
	maxFilenameLengthExceeded ConflictType = "maxFilenameLengthExceeded"
	invalidCharacters         ConflictType = "invalidCharacters"
	trailingPeriod            ConflictType = "trailingPeriod"
)

// Conflict represents a renaming operation conflict
// such as duplicate targets or empty filenames.
type Conflict struct {
	Target  string   `json:"target"`
	Cause   string   `json:"cause"`
	Sources []string `json:"sources"`
}

// newTarget appends a number to the target file name so that it
// does not conflict with an existing path on the filesystem or
// another renamed file. For example: image.png becomes image (2).png.
func newTarget(ch *Change, renamedPaths map[string][]struct {
	sourcePath string
	index      int
}) string {
	fileNoExt := utils.FilenameWithoutExtension(filepath.Base(ch.Target))
	regex := regexp.MustCompile(`\(\d+\)$`)
	// Extract the numbered index at the end of the filename (if any)
	match := regex.FindStringSubmatch(fileNoExt)
	num := 2

	if len(match) > 0 {
		_, _ = fmt.Sscanf(match[0], "(%d)", &num)
		num++
	} else {
		fileNoExt += " (" + strconv.Itoa(num) + ")"
	}

	for {
		target := regex.ReplaceAllString(fileNoExt, "("+strconv.Itoa(num)+")")
		target += filepath.Ext(ch.Target)
		target = filepath.Join(filepath.Dir(ch.Target), target)
		targetPath := filepath.Join(ch.BaseDir, target)

		// Ensure the new path does not exist on the filesystem
		if _, err := os.Stat(targetPath); err != nil &&
			errors.Is(err, os.ErrNotExist) {
			for k := range renamedPaths {
				if k == targetPath {
					goto out
				}
			}

			return target
		}
	out:
		num++
	}
}

// reportConflicts prints any detected conflicts
// to the standard output in table format.
func (op *Operation) reportConflicts() {
	var data [][]string

	if slice, exists := op.conflicts[emptyFilename]; exists {
		for _, v := range slice {
			slice := []string{
				strings.Join(v.Sources, ""),
				"",
				pterm.Red(statusEmptyFilename),
			}
			data = append(data, slice)
		}
	}

	if slice, exists := op.conflicts[trailingPeriod]; exists {
		for _, v := range slice {
			for _, s := range v.Sources {
				slice := []string{
					s,
					v.Target,
					pterm.Red(
						statusTrailingPeriod,
					),
				}
				data = append(data, slice)
			}
		}
	}

	if slice, exists := op.conflicts[fileExists]; exists {
		for _, v := range slice {
			slice := []string{
				strings.Join(v.Sources, ""),
				v.Target,
				pterm.Red(statusPathExists),
			}
			data = append(data, slice)
		}
	}

	if slice, exists := op.conflicts[overwritingNewPath]; exists {
		for _, v := range slice {
			for _, s := range v.Sources {
				slice := []string{
					s,
					v.Target,
					pterm.Red(statusOverwritingNewPath),
				}
				data = append(data, slice)
			}
		}
	}

	if slice, exists := op.conflicts[invalidCharacters]; exists {
		for _, v := range slice {
			for _, s := range v.Sources {
				slice := []string{
					s,
					v.Target,
					pterm.Red(
						fmt.Sprintf(
							string(statusInvalidCharacters),
							v.Cause,
						),
					),
				}
				data = append(data, slice)
			}
		}
	}

	if slice, exists := op.conflicts[maxFilenameLengthExceeded]; exists {
		for _, v := range slice {
			for _, s := range v.Sources {
				slice := []string{
					s,
					v.Target,
					pterm.Red(
						fmt.Sprintf(
							string(statusFilenameLengthExceeded),
							v.Cause,
						),
					),
				}
				data = append(data, slice)
			}
		}
	}

	utils.PrintTable(data, op.stdout)
}

// detectConflicts detects any conflicts that occur
// after renaming a file. Conflicts are automatically
// fixed if specified in the operation.
func (op *Operation) detectConflicts() {
	op.conflicts = make(map[ConflictType][]Conflict)

	// renamedPaths is used to detect overwriting file paths
	// after the renaming operation. The key of the map
	// is the target path.and the slice it points to must
	// have a length of 1, otherwise a conflict will be detected
	// for that target path (it means 2 or more source files are
	// being renamed to the same target)
	renamedPaths := make(map[string][]struct {
		sourcePath string
		index      int // helps keep track of source position in the op.matches slice
	})

	for i := 0; i < len(op.matches); i++ {
		ch := op.matches[i]
		sourcePath := filepath.Join(ch.BaseDir, ch.Source)
		targetPath := filepath.Join(ch.BaseDir, ch.Target)

		// Report if replacement operation results in
		// an empty string for the new filename
		if ch.Target == "." || ch.Target == "" {
			op.conflicts[emptyFilename] = append(
				op.conflicts[emptyFilename],
				Conflict{
					Sources: []string{sourcePath},
					Target:  targetPath,
				},
			)
			op.matches[i].status = statusEmptyFilename

			if op.fixConflicts {
				// The file is left unchanged
				op.matches[i].Target = ch.Source
				op.matches[i].status = statusUnchanged
			}

			continue
		}

		detected := op.checkTrailingPeriodConflict(
			sourcePath,
			ch.Target,
			targetPath,
			i,
		)
		if detected && op.fixConflicts {
			// going back an index allows rechecking the path for conflicts once more
			i--
			continue
		}

		detected = op.checkPathLengthConflict(
			sourcePath,
			ch.Target,
			targetPath,
			i,
		)
		if detected && op.fixConflicts {
			i--
			continue
		}

		detected = op.checkForbiddenCharactersConflict(
			sourcePath,
			ch.Target,
			targetPath,
			i,
		)
		if detected && op.fixConflicts {
			i--
			continue
		}

		detected = op.checkPathExistsConflict(sourcePath, targetPath, &ch, i)
		if detected && op.fixConflicts {
			i--
			continue
		}

		renamedPaths[targetPath] = append(renamedPaths[targetPath], struct {
			sourcePath string
			index      int
		}{
			sourcePath: sourcePath,
			index:      i,
		})
	}

	op.checkOverwritingPathConflict(renamedPaths)
}

// checkPathExistsConflict reports if the newly renamed path
// already exists on the filesystem.
func (op *Operation) checkPathExistsConflict(
	sourcePath, targetPath string,
	ch *Change,
	i int,
) bool {
	var conflictDetected bool
	// Report if target path exists on the filesystem
	if _, err := os.Stat(targetPath); err == nil ||
		errors.Is(err, os.ErrExist) {
		// Don't report a conflict for an unchanged filename
		// Also handles case-insensitive filesystems
		if strings.EqualFold(sourcePath, targetPath) {
			op.matches[i].status = statusUnchanged
			return false
		}

		// Don't report a conflict if overwriting files are allowed
		if op.allowOverwrites {
			op.matches[i].WillOverwrite = true
			op.matches[i].status = statusOverwriting

			return false
		}

		// Don't report a conflict if target path is changing before
		// the source path is renamed
		for j := 0; j < len(op.matches); j++ {
			ch := op.matches[j]
			sp := filepath.Join(ch.BaseDir, ch.Source)
			tp := filepath.Join(ch.BaseDir, ch.Target)

			if targetPath == sp && !strings.EqualFold(sp, tp) && i > j {
				return false
			}
		}

		op.conflicts[fileExists] = append(
			op.conflicts[fileExists],
			Conflict{
				Sources: []string{sourcePath},
				Target:  targetPath,
			},
		)

		conflictDetected = true
		op.matches[i].status = statusPathExists

		if op.fixConflicts {
			op.matches[i].Target = newTarget(ch, nil)
			op.matches[i].status = statusOK
		}
	}

	return conflictDetected
}

// checkOverwritingPathConflict ensures that a newly renamed path
// is not overwritten by another renamed file.
func (op *Operation) checkOverwritingPathConflict(
	renamedPaths map[string][]struct {
		sourcePath string
		index      int
	},
) {
	// Report duplicate targets if any
	for targetPath, source := range renamedPaths {
		if len(source) > 1 {
			var sources []string
			for _, s := range source {
				sources = append(sources, s.sourcePath)
				op.matches[s.index].status = statusOverwritingNewPath
			}

			op.conflicts[overwritingNewPath] = append(
				op.conflicts[overwritingNewPath],
				Conflict{
					Sources: sources,
					Target:  targetPath,
				},
			)

			if op.fixConflicts {
				for i := 0; i < len(source); i++ {
					item := source[i]

					if i == 0 {
						continue
					}

					target := newTarget(
						&op.matches[item.index],
						renamedPaths,
					)
					pt := filepath.Join(op.matches[item.index].BaseDir, target)

					if _, ok := renamedPaths[pt]; !ok {
						renamedPaths[pt] = []struct {
							sourcePath string
							index      int
						}{}
						op.matches[item.index].Target = target
						op.matches[item.index].status = statusOK
					} else {
						// repeat the last iteration to generate a new path
						op.matches[item.index].Target = target
						op.matches[item.index].status = statusOK
						i--
						continue
					}
				}
			}
		}
	}
}

// checkForbiddenCharacters is responsible for ensuring that target file names
// do not contain forbidden characters for the current OS.
func checkForbiddenCharacters(path string) error {
	if runtime.GOOS == Windows {
		if partialWindowsForbiddenCharRegex.MatchString(path) {
			return errors.New(
				strings.Join(
					partialWindowsForbiddenCharRegex.FindAllString(path, -1),
					",",
				),
			)
		}
	}

	if runtime.GOOS == Darwin {
		if strings.Contains(path, ":") {
			return fmt.Errorf("%s", ":")
		}
	}

	return nil
}

// checktTargetLength is responsible for ensuring that the target name length
// does not exceed the maximum value on each supported operating system.
func checktTargetLength(target string) error {
	// Get the standalone filename
	filename := filepath.Base(target)

	// max length of 260 characters in windows
	if runtime.GOOS == Windows &&
		len([]rune(filename)) > windowsMaxLength {
		return fmt.Errorf("%d characters", windowsMaxLength)
	} else if runtime.GOOS != Windows && len([]byte(filename)) > unixMaxBytes {
		// max length of 255 bytes on Linux and other unix-based OSes
		return fmt.Errorf("%d bytes", unixMaxBytes)
	}

	return nil
}

// checkTrailingPeriods reports if replacement operation results
// in files or sub directories that end in trailing dots. (windows only).
func (op *Operation) checkTrailingPeriodConflict(
	sourcePath, target, targetPath string,
	i int,
) bool {
	var conflictDetected bool

	if runtime.GOOS == Windows {
		strSlice := strings.Split(target, pathSeperator)
		for _, v := range strSlice {
			s := strings.TrimRight(v, ".")

			if s != v {
				op.conflicts[trailingPeriod] = append(
					op.conflicts[trailingPeriod],
					Conflict{
						Sources: []string{sourcePath},
						Target:  targetPath,
					},
				)
				conflictDetected = true
				op.matches[i].status = statusTrailingPeriod

				break
			}
		}

		if op.fixConflicts && conflictDetected {
			for j, v := range strSlice {
				s := strings.TrimRight(v, ".")
				strSlice[j] = s
			}

			op.matches[i].Target = strings.Join(strSlice, pathSeperator)
			op.matches[i].status = statusOK
		}
	}

	return conflictDetected
}

func (op *Operation) checkPathLengthConflict(
	sourcePath, target, targetPath string,
	i int,
) bool {
	var conflictDetected bool

	err := checktTargetLength(target)
	if err != nil {
		op.conflicts[maxFilenameLengthExceeded] = append(
			op.conflicts[maxFilenameLengthExceeded],
			Conflict{
				Sources: []string{sourcePath},
				Target:  targetPath,
				Cause:   err.Error(),
			},
		)
		conflictDetected = true
		op.matches[i].status = statusFilenameLengthExceeded

		if op.fixConflicts {
			if runtime.GOOS == Windows {
				// trim filename so that it's less than 260 characters
				filename := []rune(filepath.Base(target))
				ext := []rune(filepath.Ext(string(filename)))
				f := []rune(utils.FilenameWithoutExtension(string(filename)))
				index := windowsMaxLength - len(ext)
				f = f[:index]
				op.matches[i].Target = filepath.Join(string(f), string(ext))
			} else {
				// trim filename so that it's no more than 255 bytes
				filename := filepath.Base(target)
				ext := filepath.Ext(filename)
				fileNoExt := utils.FilenameWithoutExtension(filename)
				index := unixMaxBytes - len([]byte(ext))
				for {
					if len([]byte(fileNoExt)) > index {
						frune := []rune(fileNoExt)
						fileNoExt = string(frune[:len(frune)-1])
						continue
					}

					break
				}

				op.matches[i].Target = fileNoExt + ext
				op.matches[i].status = statusOK
			}
		}
	}

	return conflictDetected
}

func (op *Operation) checkForbiddenCharactersConflict(
	sourcePath, target, targetPath string,
	i int,
) bool {
	var conflictDetected bool

	err := checkForbiddenCharacters(target)
	if err != nil {
		op.conflicts[invalidCharacters] = append(
			op.conflicts[invalidCharacters],
			Conflict{
				Sources: []string{sourcePath},
				Target:  targetPath,
				Cause:   err.Error(),
			},
		)

		conflictDetected = true
		op.matches[i].status = statusInvalidCharacters

		if op.fixConflicts {
			if runtime.GOOS == Windows {
				op.matches[i].Target = partialWindowsForbiddenCharRegex.ReplaceAllString(
					target,
					"",
				)
			}

			if runtime.GOOS == Darwin {
				op.matches[i].Target = strings.ReplaceAll(
					target,
					":",
					"",
				)
			}

			op.matches[i].status = statusOK
		}
	}

	return conflictDetected
}