package f2

import (
	"encoding/json"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/urfave/cli/v2"
	"gopkg.in/djherbis/times.v1"
)

var fileSystem = []string{
	"No Pressure (2021) S1.E1.1080p.mkv",
	"No Pressure (2021) S1.E2.1080p.mkv",
	"No Pressure (2021) S1.E3.1080p.mkv",
	"images/a.jpg",
	"images/abc.png",
	"images/456.webp",
	"images/pics/123.JPG",
	"morepics/pic-1.avif",
	"morepics/pic-2.avif",
	"scripts/index.js",
	"scripts/main.js",
	"abc.pdf",
	"abc.epub",
	".pics",
	"conflicts/abc.txt",
	"conflicts/xyz.txt",
	"conflicts/123.txt",
	"conflicts/123 (3).txt",
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

// setupFileSystem creates all required files and folders for
// the tests and returns a function that is used as
// a teardown function when the tests are done.
func setupFileSystem(t testing.TB) (string, func()) {
	testDir, err := ioutil.TempDir(".", "")
	if err != nil {
		os.RemoveAll(testDir)
		t.Fatal(err)
	}

	directories := []string{"images/pics", "scripts", "morepics", "conflicts"}
	for _, v := range directories {
		filePath := filepath.Join(testDir, v)
		err = os.MkdirAll(filePath, os.ModePerm)
		if err != nil {
			os.RemoveAll(testDir)
			t.Fatal(err)
		}
	}

	for _, f := range fileSystem {
		filePath := filepath.Join(testDir, f)
		if err := ioutil.WriteFile(filePath, []byte{}, 0755); err != nil {
			os.RemoveAll(testDir)
			t.Fatal(err)
		}
	}

	abs, err := filepath.Abs(testDir)
	if err != nil {
		os.RemoveAll(testDir)
		t.Fatal(err)
	}

	return abs, func() {
		if os.RemoveAll(testDir); err != nil {
			t.Fatal(err)
		}
	}
}

type ActionResult struct {
	changes    []Change
	conflicts  map[conflict][]Conflict
	outputFile string
	applyError error
}

func action(args []string) (ActionResult, error) {
	var result ActionResult

	app := GetApp()
	app.Action = func(c *cli.Context) error {
		op, err := NewOperation(c)
		if err != nil {
			return err
		}

		if op.undoFile != "" {
			result.outputFile = op.undoFile
			return op.Undo()
		}

		op.FindMatches()

		if op.includeDir {
			op.SortMatches()
		}

		op.Replace()

		result.changes = op.matches

		if op.outputFile != "" {
			result.outputFile = op.outputFile
			op.WriteToFile()
		}

		result.applyError = op.Apply()
		result.conflicts = op.conflicts

		return nil
	}

	return result, app.Run(args)
}

func sortChanges(s []Change) {
	sort.Slice(s, func(i, j int) bool {
		return s[i].Source < s[j].Source
	})
}

func TestFindReplace(t *testing.T) {
	testDir, teardown := setupFileSystem(t)

	defer teardown()

	type Table struct {
		want []Change
		args []string
	}

	table := []Table{
		{
			want: []Change{
				{Source: "No Pressure (2021) S1.E1.1080p.mkv", BaseDir: testDir, Target: "1.mkv"},
				{Source: "No Pressure (2021) S1.E2.1080p.mkv", BaseDir: testDir, Target: "2.mkv"},
				{Source: "No Pressure (2021) S1.E3.1080p.mkv", BaseDir: testDir, Target: "3.mkv"},
			},
			args: []string{"-f", ".*E(\\d+).*", "-r", "$1.mkv", "-o", "map.json", testDir},
		},
		{
			want: []Change{
				{Source: "No Pressure (2021) S1.E1.1080p.mkv", BaseDir: testDir, Target: "No Pressure 98.mkv"},
				{Source: "No Pressure (2021) S1.E2.1080p.mkv", BaseDir: testDir, Target: "No Pressure 99.mkv"},
				{Source: "No Pressure (2021) S1.E3.1080p.mkv", BaseDir: testDir, Target: "No Pressure 100.mkv"},
			},
			args: []string{"-f", "(No Pressure).*", "-r", "$1 %d.mkv", "-n", "98", testDir},
		},
		{
			want: []Change{
				{Source: "a.jpg", BaseDir: filepath.Join(testDir, "images"), Target: "a.jpeg"},
			},
			args: []string{"-f", "jpg", "-r", "jpeg", "-R", testDir},
		},
		{
			want: []Change{
				{Source: "456.webp", BaseDir: filepath.Join(testDir, "images"), Target: "456-001.webp"},
				{Source: "a.jpg", BaseDir: filepath.Join(testDir, "images"), Target: "a-002.jpg"},
				{Source: "abc.png", BaseDir: filepath.Join(testDir, "images"), Target: "abc-003.png"},
			},
			args: []string{"-f", ".*(jpg|png|webp)", "-r", "{{f}}-%03d.$1", filepath.Join(testDir, "images")},
		},
		{
			want: []Change{
				{Source: "456.webp", BaseDir: filepath.Join(testDir, "images"), Target: "001.webp"},
				{Source: "a.jpg", BaseDir: filepath.Join(testDir, "images"), Target: "002.jpg"},
				{Source: "abc.png", BaseDir: filepath.Join(testDir, "images"), Target: "003.png"},
			},
			args: []string{"-f", ".*(jpg|png|webp)", "-r", "%03d{{ext}}", filepath.Join(testDir, "images")},
		},
		{
			want: []Change{
				{Source: "index.js", BaseDir: filepath.Join(testDir, "scripts"), Target: "index.ts"},
				{Source: "main.js", BaseDir: filepath.Join(testDir, "scripts"), Target: "main.ts"},
			},
			args: []string{"-f", "js", "-r", "ts", filepath.Join(testDir, "scripts")},
		},
		{
			want: []Change{
				{Source: "index.js", BaseDir: filepath.Join(testDir, "scripts"), Target: "i n d e x .js"},
				{Source: "main.js", BaseDir: filepath.Join(testDir, "scripts"), Target: "m a i n .js"},
			},
			args: []string{"-f", "(.)", "-r", "$1 ", "-e", filepath.Join(testDir, "scripts")},
		},
		{
			want: []Change{
				{Source: "a.jpg", BaseDir: filepath.Join(testDir, "images"), Target: "a.jpeg"},
				{Source: "123.JPG", BaseDir: filepath.Join(testDir, "images", "pics"), Target: "123.jpeg"},
			},
			args: []string{"-f", "jpg", "-r", "jpeg", "-R", "-i", "-o", "map.json", testDir},
		},
		{
			want: []Change{
				{Source: "pics", IsDir: true, BaseDir: filepath.Join(testDir, "images"), Target: "images"},
				{Source: "morepics", IsDir: true, BaseDir: testDir, Target: "moreimages"},
				{Source: "pic-1.avif", BaseDir: filepath.Join(testDir, "morepics"), Target: "image-1.avif"},
				{Source: "pic-2.avif", BaseDir: filepath.Join(testDir, "morepics"), Target: "image-2.avif"},
			},
			args: []string{"-f", "pic", "-r", "image", "-d", "-R", testDir},
		},
		{
			want: []Change{
				{Source: "pics", IsDir: true, BaseDir: filepath.Join(testDir, "images"), Target: "images"},
				{Source: "morepics", IsDir: true, BaseDir: testDir, Target: "moreimages"},
			},
			args: []string{"-f", "pic", "-r", "image", "-D", "-R", testDir},
		},
		{
			want: []Change{
				{Source: "No Pressure (2021) S1.E1.1080p.mkv", BaseDir: testDir, Target: "No Pressure (2022) S1.E1.1080p.mkv"},
				{Source: "No Pressure (2021) S1.E2.1080p.mkv", BaseDir: testDir, Target: "No Pressure (2022) S1.E2.1080p.mkv"},
				{Source: "No Pressure (2021) S1.E3.1080p.mkv", BaseDir: testDir, Target: "No Pressure (2022) S1.E3.1080p.mkv"},
			},
			args: []string{"-f", "(2021)", "-r", "(2022)", "-s", testDir},
		},
	}

	for i, v := range table {
		args := os.Args[0:1]
		args = append(args, v.args...)
		result, _ := action(args) // err will be nil

		if len(result.conflicts) > 0 {
			t.Fatalf("Test(%d) — Expected no conflicts but got some: %v", i+1, result.conflicts)
		}

		sortChanges(v.want)
		sortChanges(result.changes)

		if !cmp.Equal(v.want, result.changes) && len(v.want) != 0 {
			t.Fatalf("Test(%d) — Expected: %+v, got: %+v\n", i+1, v.want, result.changes)
		}

		// Test if the map file was written successfully
		if result.outputFile != "" {
			file, err := os.ReadFile(result.outputFile)
			if err != nil {
				t.Fatalf("Unexpected error when trying to read map file: %v\n", err)
			}

			var mf mapFile
			err = json.Unmarshal([]byte(file), &mf)
			if err != nil {
				t.Fatalf("Unexpected error when trying to unmarshal map file contents: %v\n", err)
			}
			ch := mf.Operations

			sortChanges(ch)

			if !cmp.Equal(v.want, ch) && len(v.want) != 0 {
				t.Fatalf("Test(%d) — Expected: %+v, got: %+v\n", i+1, v.want, ch)
			}

			err = os.Remove(result.outputFile)
			if err != nil {
				t.Log("Failed to remove output file")
			}
		}
	}
}

func TestDetectConflicts(t *testing.T) {
	testDir, teardown := setupFileSystem(t)

	defer teardown()

	type Table struct {
		want map[conflict][]Conflict
		args []string
	}

	table := []Table{
		{
			want: map[conflict][]Conflict{
				FILE_EXISTS: []Conflict{
					{
						source: []string{filepath.Join(testDir, "abc.pdf")},
						target: filepath.Join(testDir, "abc.epub"),
					},
				},
			},
			args: []string{"-f", "pdf", "-r", "epub", testDir},
		},
		{
			want: map[conflict][]Conflict{
				EMPTY_FILENAME: []Conflict{
					{
						source: []string{filepath.Join(testDir, "abc.pdf")},
						target: filepath.Join(testDir, ""),
					},
				},
			},
			args: []string{"-f", "abc.pdf", "-r", "", testDir},
		},
		{
			want: map[conflict][]Conflict{
				OVERWRITNG_NEW_PATH: []Conflict{
					{
						source: []string{filepath.Join(testDir, "abc.epub"), filepath.Join(testDir, "abc.pdf")},
						target: filepath.Join(testDir, "abc.mobi"),
					},
				},
			},
			args: []string{"-f", "pdf|epub", "-r", "mobi", testDir},
		},
	}

	for i, v := range table {
		args := os.Args[0:1]
		args = append(args, v.args...)
		result, err := action(args)
		if err != nil {
			t.Fatalf("Test(%d) — Unexpected error: %v\n", i+1, err)
		}

		if len(result.conflicts) == 0 {
			t.Fatalf("Test(%d) — Expected some conflicts but got none", i+1)
		}

		if !cmp.Equal(v.want, result.conflicts, cmp.AllowUnexported(Conflict{})) {
			t.Fatalf("Test(%d) — Expected: %+v, got: %+v\n", i+1, v.want, result.conflicts)
		}
	}
}

func TestFixConflicts(t *testing.T) {
	testDir, teardown := setupFileSystem(t)

	defer teardown()

	type Table struct {
		want []Change
		args []string
	}

	table := []Table{
		{
			want: []Change{
				{Source: "abc.txt", BaseDir: filepath.Join(testDir, "conflicts"), Target: "123 (2).txt"},
				{Source: "xyz.txt", BaseDir: filepath.Join(testDir, "conflicts"), Target: "123 (4).txt"},
			},
			args: []string{"-f", "abc|xyz", "-r", "123", "-F", filepath.Join(testDir, "conflicts")},
		},
		{
			want: []Change{
				{Source: "123.txt", BaseDir: filepath.Join(testDir, "conflicts"), Target: "abc (2).txt"},
				{Source: "123 (3).txt", BaseDir: filepath.Join(testDir, "conflicts"), Target: "abc (3).txt"},
			},
			args: []string{"-f", "123", "-r", "abc", "-F", filepath.Join(testDir, "conflicts")},
		},
		{
			want: []Change{
				{Source: "xyz.txt", BaseDir: filepath.Join(testDir, "conflicts"), Target: "123 (2).txt"},
			},
			args: []string{"-f", "xyz", "-r", "123", "-F", filepath.Join(testDir, "conflicts")},
		},
		{
			want: []Change{
				{Source: "xyz.txt", BaseDir: filepath.Join(testDir, "conflicts"), Target: "xyz.txt"},
			},
			args: []string{"-f", "xyz.txt", "-F", filepath.Join(testDir, "conflicts")},
		},
	}

	for i, v := range table {
		args := os.Args[0:1]
		args = append(args, v.args...)
		result, _ := action(args) // err will be nil

		if len(result.conflicts) == 0 {
			t.Fatalf("Test(%d) — Expected some conflicts but got none", i+1)
		}

		sortChanges(v.want)
		sortChanges(result.changes)

		if !cmp.Equal(v.want, result.changes) && len(v.want) != 0 {
			t.Fatalf("Test(%d) — Expected: %+v, got: %+v\n", i+1, v.want, result.changes)
		}
	}
}

func TestApplyUndo(t *testing.T) {
	type Table struct {
		want []Change
		exec []string
		undo []string
	}

	table := []Table{
		{
			want: []Change{
				{Source: "No Pressure (2021) S1.E1.1080p.mkv", Target: "1.mkv"},
				{Source: "No Pressure (2021) S1.E2.1080p.mkv", Target: "2.mkv"},
				{Source: "No Pressure (2021) S1.E3.1080p.mkv", Target: "3.mkv"},
			},
			exec: []string{"-f", ".*E(\\d+).*", "-r", "$1.mkv", "-o", "map.json", "-x"},
			undo: []string{"-u", "map.json", "-x"},
		},
		{
			want: []Change{
				{Source: "pics", IsDir: true, Target: "images"},
				{Source: "morepics", IsDir: true, Target: "moreimages"},
				{Source: "pic-1.avif", Target: "image-1.avif"},
				{Source: "pic-2.avif", Target: "image-2.avif"},
			},
			exec: []string{"-f", "pic", "-r", "image", "-d", "-R", "-o", "map.json", "-x"},
			undo: []string{"-u", "map.json", "-x"},
		},
	}

	for i, v := range table {
		testDir, teardown := setupFileSystem(t)

		for _, ch := range v.want {
			ch.BaseDir = testDir
		}

		v.exec = append(v.exec, testDir)

		args := os.Args[0:1]
		args = append(args, v.exec...)
		result, _ := action(args) // err will be nil

		if len(result.conflicts) > 0 {
			t.Fatalf("Test(%d) — Expected no conflicts but got some: %v", i+1, result.conflicts)
		}

		if result.applyError != nil {
			t.Fatalf("Test(%d) — Unexpected apply error: %v\n", i+1, result.applyError)
		}

		// Test Undo function
		args = os.Args[0:1]
		args = append(args, v.undo...)
		result, err := action(args)
		if err != nil {
			t.Fatalf("Test(%d) — Unexpected error in undo mode: %v\n", i+1, err)
		}

		err = os.Remove(result.outputFile)
		if err != nil {
			t.Log("Failed to remove output file")
		}

		teardown()
	}
}

func randate() time.Time {
	min := time.Date(1970, 1, 0, 0, 0, 0, 0, time.UTC).Unix()
	max := time.Date(2070, 1, 0, 0, 0, 0, 0, time.UTC).Unix()
	delta := max - min

	sec := rand.Int63n(delta) + min
	return time.Unix(sec, 0)
}

func TestReplaceDateVariables(t *testing.T) {
	testDir, teardown := setupFileSystem(t)

	defer teardown()

	for _, file := range fileSystem {
		path := filepath.Join(testDir, file)

		// change the atime and mtime to a random value
		mtime, atime := randate(), randate()
		err := os.Chtimes(path, atime, mtime)
		if err != nil {
			t.Fatalf("Expected no errors, but got one: %v\n", err)
		}

		timeInfo, err := times.Stat(path)
		if err != nil {
			t.Fatalf("Expected no errors, but got one: %v\n", err)
		}

		want := make(map[string]string)
		got := make(map[string]string)

		accessTime := timeInfo.AccessTime()
		modTime := timeInfo.ModTime()

		fileTimes := []string{"mtime", "atime", "ctime", "now", "btime"}

		for _, v := range fileTimes {
			var timeValue time.Time
			switch v {
			case "mtime":
				timeValue = modTime
			case "atime":
				timeValue = accessTime
			case "ctime":
				timeValue = modTime
				if timeInfo.HasChangeTime() {
					timeValue = timeInfo.ChangeTime()
				}
			case "btime":
				timeValue = modTime
				if timeInfo.HasBirthTime() {
					timeValue = timeInfo.BirthTime()
				}
			case "now":
				timeValue = time.Now()
			}

			for key, token := range dateTokens {
				want[v+"."+key] = timeValue.Format(token)
				out, err := replaceDateVariables(path, "{{"+v+"."+key+"}}")
				if err != nil {
					t.Fatalf("Expected no errors, but got one: %v\n", err)
				}
				got[v+"."+key] = out
			}
		}

		if !cmp.Equal(want, got) {
			t.Fatalf("Expected %v, but got %v\n", want, got)
		}
	}
}