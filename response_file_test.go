package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- helpers ----------

func writeRspFile(t *testing.T, path, content string) string {
	t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.TempDir(), path)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func writeRspFileBytes(t *testing.T, path string, content []byte) string {
	t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.TempDir(), path)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

func expandedValues(args []ExpandedArg) []string {
	values := make([]string, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	return values
}

func responseFileError(t *testing.T, err error) *ResponseFileError {
	t.Helper()
	var rfe *ResponseFileError
	require.ErrorAs(t, err, &rfe)
	return rfe
}

// ---------- scanner ----------

func TestResponseFileScanner(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		want       []string
		wantRefs   []bool
		wantStarts []rspPosition
		wantEnds   []rspPosition
	}{
		{
			name:       "simple lines",
			content:    "--verbose\n--name alice\n",
			want:       []string{"--verbose", "--name", "alice"},
			wantRefs:   []bool{false, false, false},
			wantStarts: []rspPosition{{1, 1}, {2, 1}, {2, 8}},
			wantEnds:   []rspPosition{{1, 10}, {2, 7}, {2, 13}},
		},
		{
			name:       "crlf and blank lines",
			content:    "\r\n  --x\r\n\r\n\tpos\r\n",
			want:       []string{"--x", "pos"},
			wantStarts: []rspPosition{{2, 3}, {4, 2}},
			wantEnds:   []rspPosition{{2, 6}, {4, 5}},
		},
		{
			name:       "double quotes with spaces",
			content:    `"a b c" x`,
			want:       []string{"a b c", "x"},
			wantStarts: []rspPosition{{1, 1}, {1, 9}},
			wantEnds:   []rspPosition{{1, 8}, {1, 10}},
		},
		{
			name:       "single quotes with spaces",
			content:    `'a b' x`,
			want:       []string{"a b", "x"},
			wantStarts: []rspPosition{{1, 1}, {1, 7}},
			wantEnds:   []rspPosition{{1, 6}, {1, 8}},
		},
		{
			name:       "empty quotes are kept",
			content:    `"" x`,
			want:       []string{"", "x"},
			wantStarts: []rspPosition{{1, 1}, {1, 4}},
			wantEnds:   []rspPosition{{1, 3}, {1, 5}},
		},
		{
			name:       "comments and blank lines",
			content:    "# full line comment\na # trailing\nb#notcomment\n\n'#quoted'\n",
			want:       []string{"a", "b#notcomment", "#quoted"},
			wantStarts: []rspPosition{{2, 1}, {3, 1}, {5, 1}},
			wantEnds:   []rspPosition{{2, 2}, {3, 13}, {5, 10}},
		},
		{
			name:       "escapes outside quotes",
			content:    `a\ b \@x a\\b a\#b`,
			want:       []string{"a b", "@x", `a\b`, "a#b"},
			wantStarts: []rspPosition{{1, 1}, {1, 6}, {1, 10}, {1, 15}},
		},
		{
			name:       "backslash newline joins lines",
			content:    "a\\\nb",
			want:       []string{"ab"},
			wantStarts: []rspPosition{{1, 1}},
			wantEnds:   []rspPosition{{2, 2}},
		},
		{
			name:    "single quotes keep backslashes",
			content: `'a\b\@c'`,
			want:    []string{`a\b\@c`},
		},
		{
			name:    "double quote escape rules",
			content: `"a\\b" "a\"b" "a\zb"`,
			want:    []string{`a\b`, `a"b`, `a\zb`},
		},
		{
			name:       "double quoted newline continuation",
			content:    "\"a\\\nb\"",
			want:       []string{"ab"},
			wantStarts: []rspPosition{{1, 1}},
			wantEnds:   []rspPosition{{2, 3}},
		},
		{
			name:       "multiline quoted argument",
			content:    "\"line1\nline2\" tail",
			want:       []string{"line1\nline2", "tail"},
			wantStarts: []rspPosition{{1, 1}, {2, 8}},
		},
		{
			name:     "references with quoted spaces",
			content:  `@plain.rsp @"my config.rsp" @a\ b.rsp`,
			want:     []string{"plain.rsp", "my config.rsp", "a b.rsp"},
			wantRefs: []bool{true, true, true},
		},
		{
			name:     "literal prefix forms",
			content:  `@@literal \@escaped '@single' "@double" a@mid`,
			want:     []string{"@literal", "@escaped", "@single", "@double", "a@mid"},
			wantRefs: []bool{false, false, false, false, false},
		},
		{
			name:       "utf8 rune columns",
			content:    "é x 😀 y",
			want:       []string{"é", "x", "😀", "y"},
			wantStarts: []rspPosition{{1, 1}, {1, 3}, {1, 5}, {1, 7}},
		},
		{
			name:       "bom is stripped",
			content:    "\ufeff--x y",
			want:       []string{"--x", "y"},
			wantStarts: []rspPosition{{1, 1}, {1, 5}},
		},
		{
			name:     "doubled prefix after equals is literal",
			content:  "--name=@@alice",
			want:     []string{"--name=@alice"},
			wantRefs: []bool{false},
		},
		{
			name:    "quoted value with reference after equals is literal",
			content: `--name="@alice" --name='@bob'`,
			want:    []string{"--name=@alice", "--name=@bob"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tokens, err := scanResponseFile("test.rsp", []byte(test.content), '@')
			require.Nil(t, err, "unexpected scan error: %v", err)
			require.Len(t, tokens, len(test.want))
			for i, token := range tokens {
				assert.Equal(t, test.want[i], token.value, "token %d value", i)
				if test.wantRefs != nil {
					assert.Equal(t, test.wantRefs[i], token.isRef, "token %d isRef", i)
				} else {
					assert.False(t, token.isRef, "token %d should not be a ref", i)
				}
				if test.wantStarts != nil {
					assert.Equal(t, test.wantStarts[i], token.start, "token %d start", i)
				}
				if test.wantEnds != nil {
					assert.Equal(t, test.wantEnds[i], token.end, "token %d end", i)
				}
			}
		})
	}
}

func TestResponseFileScannerErrors(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		kind     ResponseFileErrorKind
		pos      rspPosition
		flagName string
	}{
		{"unterminated double quote", `"abc`, ResponseFileErrorSyntax, rspPosition{1, 1}, ""},
		{"unterminated single quote multiline", "'abc\ndef", ResponseFileErrorSyntax, rspPosition{1, 1}, ""},
		{"dangling backslash", `abc\`, ResponseFileErrorSyntax, rspPosition{1, 4}, ""},
		{"dangling backslash in double quotes", `"abc\`, ResponseFileErrorSyntax, rspPosition{1, 5}, ""},
		{"invalid utf8", "a\xFF\n", ResponseFileErrorEncoding, rspPosition{1, 2}, ""},
		{"invalid utf8 later line", "ab\nc\xFF", ResponseFileErrorEncoding, rspPosition{2, 2}, ""},
		{"bare reference after equals", "--name=@x", ResponseFileErrorReferenceNotAllowed, rspPosition{1, 8}, "--name"},
		{"bare reference after short equals", "-n=@x", ResponseFileErrorReferenceNotAllowed, rspPosition{1, 4}, "-n"},
		{"empty reference", `@`, ResponseFileErrorEmptyReference, rspPosition{1, 1}, ""},
		{"empty quoted reference", `@""`, ResponseFileErrorEmptyReference, rspPosition{1, 1}, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := scanResponseFile("test.rsp", []byte(test.content), '@')
			require.NotNil(t, err)
			assert.Equal(t, test.kind, err.Kind)
			assert.Equal(t, test.pos.line, err.Source.Line, "line")
			assert.Equal(t, test.pos.col, err.Source.Column, "column")
			if test.flagName != "" {
				assert.Equal(t, test.flagName, err.Flag)
			}
		})
	}
}

// ---------- public expansion API ----------

func TestExpandResponseFilesDisabled(t *testing.T) {
	args := []string{"app", "--flag", "@does-not-exist.rsp", "plain"}
	expanded, err := ExpandResponseFiles(args, ResponseFileConfig{})
	require.NoError(t, err)
	assert.Equal(t, args, expandedValues(expanded))
	for i, arg := range expanded {
		assert.True(t, arg.Source.IsCommandLine(), "arg %d should be command line sourced", i)
		assert.Equal(t, i+1, arg.Source.Column)
	}
}

func TestExpandResponseFilesBasic(t *testing.T) {
	dir := t.TempDir()
	main := writeRspFile(t, filepath.Join(dir, "main.rsp"),
		"# top comment\n--name alice\n--verbose\npos1\n@second.rsp\npos2\n")
	writeRspFile(t, filepath.Join(dir, "second.rsp"),
		"  --count 3\r\nnested-pos\n")

	expanded, err := ExpandResponseFiles(
		[]string{"app", "@" + main, "direct-pos"},
		ResponseFileConfig{Enabled: true},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"app",
		"--name", "alice", "--verbose", "pos1",
		"--count", "3", "nested-pos",
		"pos2",
		"direct-pos",
	}, expandedValues(expanded))

	// Sources: the program name and direct arg come from the command line,
	// everything else from a file with exact line/column positions.
	assert.True(t, expanded[0].Source.IsCommandLine())
	assert.Equal(t, "app", expanded[0].Value)

	nameFlag := expanded[1]
	assert.Equal(t, main, nameFlag.Source.File)
	assert.Equal(t, 2, nameFlag.Source.Line)
	assert.Equal(t, 1, nameFlag.Source.Column)
	assert.NotNil(t, nameFlag.Source.IncludedFrom)
	assert.True(t, nameFlag.Source.IncludedFrom.IsCommandLine())
	assert.Equal(t, 2, nameFlag.Source.IncludedFrom.Column)

	countValue := expanded[6]
	assert.Equal(t, filepath.Join(dir, "second.rsp"), countValue.Source.File)
	assert.Equal(t, 1, countValue.Source.Line)
	assert.Equal(t, 11, countValue.Source.Column)
	require.NotNil(t, countValue.Source.IncludedFrom)
	assert.Equal(t, main, countValue.Source.IncludedFrom.File)
	assert.Equal(t, 5, countValue.Source.IncludedFrom.Line)

	assert.True(t, expanded[len(expanded)-1].Source.IsCommandLine())
}

func TestExpandResponseFilesMultipleAndInterleaving(t *testing.T) {
	dir := t.TempDir()
	f1 := writeRspFile(t, filepath.Join(dir, "f1.rsp"), "a b")
	f2 := writeRspFile(t, filepath.Join(dir, "f2.rsp"), "c\nd")

	expanded, err := ExpandResponseFiles(
		[]string{"app", "x", "@" + f1, "y", "@" + f2, "z"},
		ResponseFileConfig{Enabled: true},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"app", "x", "a", "b", "y", "c", "d", "z"}, expandedValues(expanded))
}

func TestExpandResponseFilesRelativeToIncludingFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	inner := writeRspFile(t, filepath.Join(dir, "sub", "inner.rsp"), "hello")
	outer := writeRspFile(t, filepath.Join(dir, "sub", "outer.rsp"), "@inner.rsp\n")

	expanded, err := ExpandResponseFiles(
		[]string{"app", "@" + outer},
		ResponseFileConfig{Enabled: true},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"app", "hello"}, expandedValues(expanded))
	assert.Equal(t, inner, expanded[1].Source.File)
}

func TestExpandResponseFilesIncludeChain(t *testing.T) {
	dir := t.TempDir()
	c := writeRspFile(t, filepath.Join(dir, "c.rsp"), "deep")
	b := writeRspFile(t, filepath.Join(dir, "b.rsp"), "@"+c+"\n")
	a := writeRspFile(t, filepath.Join(dir, "a.rsp"), "@"+b+"\n")

	expanded, err := ExpandResponseFiles(
		[]string{"app", "@" + a},
		ResponseFileConfig{Enabled: true},
	)
	require.NoError(t, err)
	src := expanded[1].Source
	assert.Equal(t, c, src.File)
	require.NotNil(t, src.IncludedFrom)
	assert.Equal(t, b, src.IncludedFrom.File)
	require.NotNil(t, src.IncludedFrom.IncludedFrom)
	assert.Equal(t, a, src.IncludedFrom.IncludedFrom.File)
	require.NotNil(t, src.IncludedFrom.IncludedFrom.IncludedFrom)
	assert.True(t, src.IncludedFrom.IncludedFrom.IncludedFrom.IsCommandLine())
	assert.Contains(t, src.String(), c)
	assert.Contains(t, src.String(), "referenced from")
}

func TestExpandResponseFilesDiamondAllowed(t *testing.T) {
	dir := t.TempDir()
	d := writeRspFile(t, filepath.Join(dir, "d.rsp"), "shared")
	b := writeRspFile(t, filepath.Join(dir, "b.rsp"), "@"+d+"\n")
	c := writeRspFile(t, filepath.Join(dir, "c.rsp"), "@"+d+"\n")
	a := writeRspFile(t, filepath.Join(dir, "a.rsp"), "@"+b+"\n@"+c+"\n")

	expanded, err := ExpandResponseFiles(
		[]string{"app", "@" + a},
		ResponseFileConfig{Enabled: true},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"app", "shared", "shared"}, expandedValues(expanded))
}

func TestExpandResponseFilesCustomPrefix(t *testing.T) {
	dir := t.TempDir()
	f := writeRspFile(t, filepath.Join(dir, "p.rsp"), "expanded")
	expanded, err := ExpandResponseFiles(
		[]string{"app", "%" + f, "@literal"},
		ResponseFileConfig{Enabled: true, Prefix: "%"},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"app", "expanded", "@literal"}, expandedValues(expanded))
}

func TestExpandResponseFilesInvalidPrefix(t *testing.T) {
	_, err := ExpandResponseFiles(
		[]string{"app"},
		ResponseFileConfig{Enabled: true, Prefix: "@@"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Prefix")
}

func TestExpandResponseFilesProgramNameNeverExpanded(t *testing.T) {
	dir := t.TempDir()
	f := writeRspFile(t, filepath.Join(dir, "args.rsp"), "ran")
	// Program name uses the prefix but must not be read as a file.
	expanded, err := ExpandResponseFiles(
		[]string{filepath.Join(dir, "@myapp"), "@" + f},
		ResponseFileConfig{Enabled: true},
	)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "@myapp"), expanded[0].Value)
	assert.Equal(t, []string{"ran"}, expandedValues(expanded[1:]))
}

// ---------- error cases ----------

func TestExpandResponseFileNotFound(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.rsp")

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + missing},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorNotFound, rfe.Kind)
	assert.True(t, rfe.Source.IsCommandLine())
	assert.Equal(t, 2, rfe.Source.Column)
	assert.True(t, errors.Is(err, os.ErrNotExist))
	assert.Contains(t, err.Error(), "command line argument 2")
}

func TestExpandResponseFileNotFoundInFileKeepsChain(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.rsp")
	outer := writeRspFile(t, filepath.Join(dir, "outer.rsp"), "# c\n@"+missing+"\n")

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + outer},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorNotFound, rfe.Kind)
	assert.Equal(t, outer, rfe.Source.File)
	assert.Equal(t, 2, rfe.Source.Line)
	assert.Equal(t, 1, rfe.Source.Column)
	require.NotNil(t, rfe.Source.IncludedFrom)
	assert.True(t, rfe.Source.IncludedFrom.IsCommandLine())
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestExpandResponseFileRecursiveSelf(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "self.rsp")
	writeRspFile(t, self, "@"+self+"\n")

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + self},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorRecursive, rfe.Kind)
	assert.Contains(t, err.Error(), "references itself")
}

func TestExpandResponseFileCircular(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.rsp")
	b := filepath.Join(dir, "b.rsp")
	writeRspFile(t, a, "@"+b+"\n")
	writeRspFile(t, b, "@"+a+"\n")

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + a},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorCircular, rfe.Kind)
	assert.Contains(t, rfe.detail, a)
	assert.Contains(t, rfe.detail, b)
	assert.Contains(t, err.Error(), "cycle")
	// The offending reference lives in b.rsp at line 1.
	assert.Equal(t, b, rfe.Source.File)
	assert.Equal(t, 1, rfe.Source.Line)
}

func TestExpandResponseFileDepthLimit(t *testing.T) {
	dir := t.TempDir()
	b := writeRspFile(t, filepath.Join(dir, "b.rsp"), "x")
	a := writeRspFile(t, filepath.Join(dir, "a.rsp"), "@"+b+"\n")

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + a},
		ResponseFileConfig{Enabled: true, MaxDepth: 1},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorDepth, rfe.Kind)
	assert.Equal(t, int64(1), rfe.Limit)
	assert.Equal(t, int64(2), rfe.Actual)
	assert.Equal(t, a, rfe.Source.File)
}

func TestExpandResponseFileSizeLimit(t *testing.T) {
	dir := t.TempDir()
	big := writeRspFile(t, filepath.Join(dir, "big.rsp"), strings.Repeat("x", 100))

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + big},
		ResponseFileConfig{Enabled: true, MaxFileSize: 50},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorSize, rfe.Kind)
	assert.Equal(t, int64(50), rfe.Limit)
	assert.Equal(t, int64(100), rfe.Actual)
}

func TestExpandResponseFileTotalSizeLimit(t *testing.T) {
	dir := t.TempDir()
	f1 := writeRspFile(t, filepath.Join(dir, "f1.rsp"), strings.Repeat("a", 30))
	f2 := writeRspFile(t, filepath.Join(dir, "f2.rsp"), strings.Repeat("b", 30))

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + f1, "@" + f2},
		ResponseFileConfig{Enabled: true, MaxFileSize: 1000, MaxTotalSize: 40},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorSize, rfe.Kind)
	assert.Equal(t, int64(40), rfe.Limit)
	assert.Equal(t, int64(60), rfe.Actual)
	// The failing reference is the third command line argument.
	assert.True(t, rfe.Source.IsCommandLine())
	assert.Equal(t, 3, rfe.Source.Column)
}

func TestExpandResponseFileArgsLimit(t *testing.T) {
	dir := t.TempDir()
	f := writeRspFile(t, filepath.Join(dir, "many.rsp"), "a b c d")

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + f},
		ResponseFileConfig{Enabled: true, MaxExpandedArgs: 3},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorArgsLimit, rfe.Kind)
	assert.Equal(t, int64(3), rfe.Limit)
}

func TestExpandResponseFileInvalidEncoding(t *testing.T) {
	dir := t.TempDir()
	f := writeRspFileBytes(t, filepath.Join(dir, "bad.rsp"), []byte("--x\nSUPERSECRET \xFF"))

	_, err := ExpandResponseFiles(
		[]string{"app", "@" + f},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorEncoding, rfe.Kind)
	assert.Equal(t, 2, rfe.Source.Line)
	assert.Equal(t, 13, rfe.Source.Column)
	// Diagnostics never carry file contents.
	assert.NotContains(t, err.Error(), "SUPERSECRET")
}

func TestExpandResponseFileEmptyReference(t *testing.T) {
	dir := t.TempDir()
	f := writeRspFile(t, filepath.Join(dir, "empty.rsp"), "x\n@\n")

	_, err := ExpandResponseFiles(
		[]string{"app", "@"},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorEmptyReference, rfe.Kind)
	assert.True(t, rfe.Source.IsCommandLine())

	_, err = ExpandResponseFiles(
		[]string{"app", "@" + f},
		ResponseFileConfig{Enabled: true},
	)
	rfe = responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorEmptyReference, rfe.Kind)
	assert.Equal(t, f, rfe.Source.File)
	assert.Equal(t, 2, rfe.Source.Line)
}

func TestExpandResponseFileReferenceNotAllowedInArgv(t *testing.T) {
	_, err := ExpandResponseFiles(
		[]string{"app", "--name=@secret.rsp"},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorReferenceNotAllowed, rfe.Kind)
	assert.Equal(t, "--name", rfe.Flag)
	// The source locates the command line *argument* containing the reference.
	assert.Equal(t, 2, rfe.Source.Column)

	// Doubled prefix yields a literal value.
	expanded, err := ExpandResponseFiles(
		[]string{"app", "--name=@@alice"},
		ResponseFileConfig{Enabled: true},
	)
	require.NoError(t, err)
	assert.Equal(t, "--name=@alice", expanded[1].Value)
}

func TestExpandResponseDirectoryIsNotReadable(t *testing.T) {
	dir := t.TempDir()
	_, err := ExpandResponseFiles(
		[]string{"app", "@" + dir},
		ResponseFileConfig{Enabled: true},
	)
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorNotReadable, rfe.Kind)
}

// ---------- Command integration ----------

func newResponseFileCommand(t *testing.T) (*Command, *bytes.Buffer, *bytes.Buffer, map[string]any) {
	values := map[string]any{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := &Command{
		Name:      "app",
		Writer:    out,
		ErrWriter: errOut,
		Flags: []Flag{
			&BoolFlag{Name: "verbose"},
			&StringFlag{Name: "name"},
		},
		Commands: []*Command{
			{
				Name: "sub",
				Flags: []Flag{
					&StringFlag{Name: "region", Aliases: []string{"r"}},
				},
				Action: func(_ context.Context, c *Command) error {
					values["sub-region"] = c.String("region")
					return nil
				},
			},
		},
		Action: func(_ context.Context, c *Command) error {
			values["name"] = c.String("name")
			values["verbose"] = c.Bool("verbose")
			values["args"] = c.Args().Slice()
			return nil
		},
	}
	return cmd, out, errOut, values
}

func TestCommandResponseFileEndToEnd(t *testing.T) {
	dir := t.TempDir()

	t.Run("flags and positionals", func(t *testing.T) {
		rsp := writeRspFile(t, filepath.Join(dir, "args.rsp"),
			"# configure everything from a file\n--name alice\n--verbose\npos1\n")

		cmd, _, _, values := newResponseFileCommand(t)
		cmd.ResponseFile = ResponseFileConfig{Enabled: true}

		require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "@" + rsp, "pos2"}))
		assert.Equal(t, "alice", values["name"])
		assert.Equal(t, true, values["verbose"])
		assert.Equal(t, []string{"pos1", "pos2"}, values["args"])

		expanded := cmd.ExpandedArgs()
		require.NotNil(t, expanded)
		var fromFile int
		for _, arg := range expanded[1:] {
			if !arg.Source.IsCommandLine() && arg.Source.File != stdinSourceFile {
				fromFile++
				assert.NotZero(t, arg.Source.Line)
			}
		}
		assert.Greater(t, fromFile, 0)
	})

	t.Run("subcommand and subcommand flags", func(t *testing.T) {
		rsp := writeRspFile(t, filepath.Join(dir, "sub.rsp"),
			"# dispatch a subcommand\nsub --region east\n")

		cmd, _, _, values := newResponseFileCommand(t)
		cmd.ResponseFile = ResponseFileConfig{Enabled: true}

		require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "@" + rsp}))
		assert.Equal(t, "east", values["sub-region"])
	})
}

func TestCommandResponseFileDisabledKeepsArgvSemantics(t *testing.T) {
	cmd, _, _, values := newResponseFileCommand(t)

	// Feature off: an argument that looks like a reference is positional,
	// no files are touched.
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "@no-such-file.rsp"}))
	assert.Equal(t, []string{"@no-such-file.rsp"}, values["args"])
	assert.Nil(t, cmd.ExpandedArgs())
}

func TestCommandResponseFileSameValidationAsArgv(t *testing.T) {
	dir := t.TempDir()

	t.Run("unknown flag from file", func(t *testing.T) {
		rsp := writeRspFile(t, filepath.Join(dir, "unknown.rsp"), "--nope x\n")
		cmd, _, errOut, values := newResponseFileCommand(t)
		cmd.ResponseFile = ResponseFileConfig{Enabled: true}

		err := cmd.Run(buildTestContext(t), []string{"app", "@" + rsp})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "flag provided but not defined")
		assert.Contains(t, errOut.String(), "Incorrect Usage")
		assert.NotContains(t, values, "name")
	})

	t.Run("required flag still enforced", func(t *testing.T) {
		rsp := writeRspFile(t, filepath.Join(dir, "required.rsp"), "positional\n")
		out := &bytes.Buffer{}
		ran := false
		cmd := &Command{
			Name:      "app",
			Writer:    out,
			ErrWriter: &bytes.Buffer{},
			Flags: []Flag{
				&StringFlag{Name: "mandatory", Required: true},
			},
			ResponseFile: ResponseFileConfig{Enabled: true},
			Action: func(_ context.Context, _ *Command) error {
				ran = true
				return nil
			},
		}
		err := cmd.Run(buildTestContext(t), []string{"app", "@" + rsp})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Required flag")
		assert.False(t, ran)
	})
}

func TestCommandResponseFileHelpDoesNotLeakValues(t *testing.T) {
	dir := t.TempDir()
	rsp := writeRspFile(t, filepath.Join(dir, "help.rsp"),
		"--help\n--name SUPERSECRETVALUE\n")

	cmd, out, errOut, _ := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}
	ran := false
	cmd.Action = func(_ context.Context, _ *Command) error {
		ran = true
		return nil
	}

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "@" + rsp}))
	assert.False(t, ran, "help request must not run the action")
	assert.Contains(t, out.String(), "GLOBAL OPTIONS")
	assert.Contains(t, out.String(), "--name")
	assert.NotContains(t, out.String(), "SUPERSECRETVALUE")
	assert.NotContains(t, errOut.String(), "SUPERSECRETVALUE")
}

func TestCommandResponseFileExpansionErrorDoesNotLeakAndAbortsBeforeRun(t *testing.T) {
	dir := t.TempDir()
	// The file mentions a secret-looking value before the failing include.
	rsp := writeRspFile(t, filepath.Join(dir, "broken.rsp"),
		"--name SUPERSECRETVALUE\n@"+filepath.Join(dir, "missing.rsp")+"\n")

	beforeRan, actionRan := false, false
	cmd, _, errOut, _ := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}
	cmd.Before = func(_ context.Context, _ *Command) (context.Context, error) {
		beforeRan = true
		return nil, nil
	}
	cmd.Action = func(_ context.Context, _ *Command) error {
		actionRan = true
		return nil
	}

	err := cmd.Run(buildTestContext(t), []string{"app", "@" + rsp})
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorNotFound, rfe.Kind)
	assert.False(t, beforeRan)
	assert.False(t, actionRan)
	assert.NotContains(t, err.Error(), "SUPERSECRETVALUE")
	assert.Empty(t, errOut.String())
}

func TestCommandResponseFileFlagValueAsSeparateArgument(t *testing.T) {
	dir := t.TempDir()
	secret := writeRspFile(t, filepath.Join(dir, "secret.rsp"), "alice")
	cmd, _, _, values := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--name", "@" + secret}))
	assert.Equal(t, "alice", values["name"])
}

func TestCommandResponseFileRejectsReferenceAfterEquals(t *testing.T) {
	cmd, _, errOut, values := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}

	err := cmd.Run(buildTestContext(t), []string{"app", "--name=@whatever.rsp"})
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorReferenceNotAllowed, rfe.Kind)
	assert.Equal(t, "--name", rfe.Flag)
	assert.NotContains(t, values, "name")
	assert.Empty(t, errOut.String())
}

func TestCommandResponseFileCompletion(t *testing.T) {
	dir := t.TempDir()
	// Completing "app sub --reg" should offer the subcommand flag.
	rsp := writeRspFile(t, filepath.Join(dir, "complete.rsp"), "sub --reg")

	out := &bytes.Buffer{}
	ran := false
	cmd := &Command{
		Name:                  "app",
		EnableShellCompletion: true,
		Writer:                out,
		ErrWriter:             &bytes.Buffer{},
		ResponseFile:          ResponseFileConfig{Enabled: true},
		Commands: []*Command{
			{
				Name: "sub",
				Flags: []Flag{
					&StringFlag{Name: "region"},
				},
				Action: func(_ context.Context, _ *Command) error {
					ran = true
					return nil
				},
			},
		},
	}

	err := cmd.Run(buildTestContext(t), []string{"app", "@" + rsp, completionFlag})
	require.NoError(t, err)
	assert.False(t, ran, "completion must not execute the action")
	assert.Contains(t, out.String(), "--region")
}

func TestCommandResponseFileDoubleDashKeepsContentsPositional(t *testing.T) {
	dir := t.TempDir()
	rsp := writeRspFile(t, filepath.Join(dir, "dd.rsp"),
		"--verbose\n--\n--name from-file\n")

	cmd, _, _, values := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "@" + rsp}))
	// Everything after the "--" stays positional even though it came from a file.
	assert.Equal(t, []string{"--name", "from-file"}, values["args"])
	assert.Equal(t, true, values["verbose"])
	assert.Equal(t, "", values["name"])
}

func TestCommandResponseFileReferenceAfterDoubleDashExpands(t *testing.T) {
	dir := t.TempDir()
	rsp := writeRspFile(t, filepath.Join(dir, "pos.rsp"), "file-positional")

	cmd, _, _, values := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--", "@" + rsp}))
	// The reference expands even after "--"; the parser then treats the
	// expanded contents as positional arguments.
	assert.Equal(t, []string{"file-positional"}, values["args"])
}

func TestCommandResponseFileLimitEnforcedAtRuntime(t *testing.T) {
	dir := t.TempDir()
	rsp := writeRspFile(t, filepath.Join(dir, "many.rsp"), "a b c d e")
	cmd, _, _, _ := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true, MaxExpandedArgs: 2}

	err := cmd.Run(buildTestContext(t), []string{"app", "@" + rsp})
	rfe := responseFileError(t, err)
	assert.Equal(t, ResponseFileErrorArgsLimit, rfe.Kind)
}

func TestCommandResponseFileProgramNameIgnored(t *testing.T) {
	dir := t.TempDir()
	rsp := writeRspFile(t, filepath.Join(dir, "args.rsp"), "pos")
	cmd, _, _, values := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}

	require.NoError(t, cmd.Run(buildTestContext(t), []string{filepath.Join(dir, "@oddprog"), "@" + rsp}))
	assert.Equal(t, []string{"pos"}, values["args"])
}

func TestCommandResponseFilesWithStdin(t *testing.T) {
	dir := t.TempDir()
	rsp := writeRspFile(t, filepath.Join(dir, "args.rsp"), "from-file")
	cmd, _, _, values := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}
	cmd.ReadArgsFromStdin = true
	cmd.Reader = strings.NewReader("from-stdin\n")

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "@" + rsp}))
	assert.Equal(t, []string{"from-file", "from-stdin"}, values["args"])
	expanded := cmd.ExpandedArgs()
	require.NotNil(t, expanded)
	assert.Equal(t, stdinSourceFile, expanded[len(expanded)-1].Source.File)
}

func TestCommandResponseFileTraceDoesNotLeak(t *testing.T) {
	oldErr := os.Stderr
	oldTracing := isTracingOn
	defer func() {
		os.Stderr = oldErr
		isTracingOn = oldTracing
	}()

	traceFile, err := os.CreateTemp(t.TempDir(), "trace*")
	require.NoError(t, err)
	os.Stderr = traceFile
	isTracingOn = true

	dir := t.TempDir()
	rsp := writeRspFile(t, filepath.Join(dir, "secret.rsp"), "--name TRACESECRET\n")
	cmd, _, _, _ := newResponseFileCommand(t)
	cmd.ResponseFile = ResponseFileConfig{Enabled: true}
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "@" + rsp}))

	require.NoError(t, traceFile.Close())
	traceBytes, err := os.ReadFile(traceFile.Name())
	require.NoError(t, err)
	assert.NotContains(t, string(traceBytes), "TRACESECRET")
	assert.Contains(t, string(traceBytes), "argument from")
}

func TestArgSourceLocationFormatting(t *testing.T) {
	assert.Equal(t, "command line argument 3", ArgSource{Column: 3}.Location())
	assert.Equal(t, "stdin argument 2", ArgSource{File: stdinSourceFile, Column: 2}.Location())
	assert.Equal(t, "x.rsp:4:7", ArgSource{File: "x.rsp", Line: 4, Column: 7}.Location())

	parent := &ArgSource{File: "outer.rsp", Line: 1, Column: 1}
	child := ArgSource{File: "inner.rsp", Line: 2, Column: 3, IncludedFrom: parent}
	s := child.String()
	assert.Contains(t, s, "inner.rsp:2:3")
	assert.Contains(t, s, "outer.rsp:1:1")
	assert.True(t, strings.HasPrefix(s, "inner.rsp:2:3"))

	// Error rendering always includes a location.
	err := &ResponseFileError{Kind: ResponseFileErrorDepth, Limit: 1, Actual: 2, Source: child}
	assert.Contains(t, fmt.Sprint(err), "inner.rsp:2:3")
	assert.Contains(t, fmt.Sprint(err), "outer.rsp:1:1")
}
