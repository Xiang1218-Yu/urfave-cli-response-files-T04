package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Response file (argument file) support.
//
// When enabled on the root command, an argument that starts with the
// configured prefix (by default "@") names a response file whose contents
// are expanded in place of the argument. Response files use shell-like
// syntax:
//
//   - Arguments are separated by any Unicode whitespace. Blank lines are
//     allowed and carry no meaning.
//   - Double quotes (") and single quotes (') group arguments, including
//     whitespace and newlines. The quotes themselves are removed.
//   - Outside quotes, a backslash escapes the next rune. Inside double
//     quotes only the backslash, double quote and newline are escapable;
//     other backslashes are kept verbatim. Inside single quotes no escape
//     processing happens.
//   - A "#" that begins an unquoted argument starts a comment that runs to
//     the end of its line. "#" appearing anywhere else is literal.
//   - A reference is an unquoted argument that begins with the prefix, for
//     example "@config.rsp". The remainder of the argument is the file
//     name and may itself use quoting and escaping, for example
//     @"my config.rsp". References may appear anywhere a command, flag or
//     positional argument may appear, including inside another response
//     file; inclusions are recursive.
//   - To pass a literal argument that starts with the prefix, double it
//     ("@@literal" becomes "@literal"), escape it ("\@literal"), or quote
//     it ("'@literal'").
//
// Expansion happens once, before parsing, so expanded arguments go through
// exactly the same flag parsing, validation, help and completion logic as
// arguments passed directly by the operating system. Every expanded
// argument retains the file, line and column it came from, which is
// available from [Command.ExpandedArgs] and from expansion errors.
//
// Expansion is opt-in: with [ResponseFileConfig.Enabled] left false,
// arguments are passed to parsing byte for byte as before.

const (
	stdinSourceFile = "<stdin>"

	defaultResponseFilePrefix    = "@"
	defaultResponseFileDepth     = 10
	defaultResponseFileSize      = 64 * 1024
	defaultResponseFileTotalSize = 1024 * 1024
	defaultResponseFileArgCount  = 10_000
	responseFileBOM              = "\ufeff"
)

// ResponseFileConfig configures optional expansion of response file
// references (such as "@args.rsp") in the arguments passed to the root
// [Command]. It is only read on the root command.
type ResponseFileConfig struct {
	// Enabled turns on response file expansion. The zero value keeps the
	// original operating system argument semantics: arguments beginning
	// with the prefix are passed through untouched.
	Enabled bool `json:"enabled"`
	// Prefix is the rune that introduces a response file reference. It
	// must be a single rune and defaults to "@".
	Prefix string `json:"prefix"`
	// MaxDepth is the maximum allowed response file nesting depth. A file
	// referenced directly from the command line is depth 1. Zero or
	// negative values fall back to a default of 10.
	MaxDepth int `json:"maxDepth"`
	// MaxFileSize is the maximum size, in bytes, of a single response
	// file that will be read. Zero or negative values fall back to a
	// default of 64 KiB.
	MaxFileSize int64 `json:"maxFileSize"`
	// MaxTotalSize is the maximum total size, in bytes, read across all
	// response files during one expansion. Zero or negative values fall
	// back to a default of 1 MiB.
	MaxTotalSize int64 `json:"maxTotalSize"`
	// MaxExpandedArgs is the maximum number of arguments produced by one
	// expansion, including arguments passed on the command line. Zero or
	// negative values fall back to a default of 10000.
	MaxExpandedArgs int `json:"maxExpandedArgs"`
}

// ArgSource records where an expanded argument came from.
type ArgSource struct {
	// File is the path of the response file the argument came from. It is
	// empty for arguments passed directly on the command line and is
	// "<stdin>" for arguments read from standard input.
	File string `json:"file"`
	// Line is the 1-based line on which the argument starts. It is zero
	// for command line and standard input arguments.
	Line int `json:"line"`
	// Column is the 1-based rune column at which the argument starts. For
	// command line and standard input arguments it is the 1-based
	// position of the argument.
	Column int `json:"column"`
	// EndColumn is the 1-based rune column immediately after the last
	// rune of the argument on its starting line.
	EndColumn int `json:"endColumn"`
	// IncludedFrom is the location of the reference that caused File to be
	// included, or nil for command line and standard input arguments.
	IncludedFrom *ArgSource `json:"-"`
}

// IsCommandLine reports whether the argument was passed directly on the
// command line rather than read from a response file or standard input.
func (s ArgSource) IsCommandLine() bool {
	return s.File == ""
}

// Location renders the location as "path:line:column" for response file
// arguments, or as "command line argument N" / "stdin argument N".
func (s ArgSource) Location() string {
	switch {
	case s.File == stdinSourceFile:
		return fmt.Sprintf("stdin argument %d", s.Column)
	case s.File == "":
		return fmt.Sprintf("command line argument %d", s.Column)
	case s.Line > 0:
		return fmt.Sprintf("%s:%d:%d", s.File, s.Line, s.Column)
	default:
		return s.File
	}
}

// String renders the location followed by the chain of references that
// included it, outermost last.
func (s ArgSource) String() string {
	locations := []string{s.Location()}
	for parent := s.IncludedFrom; parent != nil; parent = parent.IncludedFrom {
		locations = append(locations, parent.Location())
	}
	if len(locations) == 1 {
		return locations[0]
	}
	var b strings.Builder
	b.WriteString(locations[0])
	for _, loc := range locations[1:] {
		b.WriteString(", referenced from ")
		b.WriteString(loc)
	}
	return b.String()
}

// ExpandedArg is a single expanded argument together with its source.
type ExpandedArg struct {
	// Value is the argument as seen by command and flag parsing.
	Value string `json:"value"`
	// Source is where the argument came from.
	Source ArgSource `json:"source"`
}

// ResponseFileErrorKind classifies a [ResponseFileError].
type ResponseFileErrorKind string

const (
	// ResponseFileErrorNotFound means a referenced file could not be
	// opened. The wrapped error is available via errors.Unwrap.
	ResponseFileErrorNotFound ResponseFileErrorKind = "not_found"
	// ResponseFileErrorNotReadable means the file exists but could not be
	// read (for example it is a directory).
	ResponseFileErrorNotReadable ResponseFileErrorKind = "not_readable"
	// ResponseFileErrorRecursive means a response file references itself
	// directly.
	ResponseFileErrorRecursive ResponseFileErrorKind = "recursive_reference"
	// ResponseFileErrorCircular means a chain of response file
	// references loops back to a file already being expanded.
	ResponseFileErrorCircular ResponseFileErrorKind = "circular_reference"
	// ResponseFileErrorDepth means the nesting depth limit was exceeded.
	ResponseFileErrorDepth ResponseFileErrorKind = "depth_limit"
	// ResponseFileErrorSize means a single file or the total of all files
	// exceeded the configured size limit.
	ResponseFileErrorSize ResponseFileErrorKind = "size_limit"
	// ResponseFileErrorArgsLimit means too many arguments were produced.
	ResponseFileErrorArgsLimit ResponseFileErrorKind = "args_limit"
	// ResponseFileErrorEncoding means a file contains invalid UTF-8.
	ResponseFileErrorEncoding ResponseFileErrorKind = "invalid_encoding"
	// ResponseFileErrorSyntax means a file has unterminated quotes or a
	// dangling escape.
	ResponseFileErrorSyntax ResponseFileErrorKind = "invalid_syntax"
	// ResponseFileErrorEmptyReference means a reference did not name a
	// file.
	ResponseFileErrorEmptyReference ResponseFileErrorKind = "empty_reference"
	// ResponseFileErrorReferenceNotAllowed means a reference was written
	// in a position that cannot refer to a file, such as the value part
	// of "--flag=@file".
	ResponseFileErrorReferenceNotAllowed ResponseFileErrorKind = "reference_not_allowed"
)

// ResponseFileError is returned, before any command or flag parsing runs,
// when response file expansion fails.
//
// Error messages intentionally never contain response file contents, so
// values such as secrets stored in response files are not leaked through
// diagnostics.
type ResponseFileError struct {
	// Kind classifies the error.
	Kind ResponseFileErrorKind
	// Source is the location of the offending reference or token.
	Source ArgSource
	// Path is the resolved response file path, when relevant.
	Path string
	// Flag is the flag token (for example "--name") for
	// ResponseFileErrorReferenceNotAllowed. Only the flag name is carried,
	// never the value.
	Flag string
	// Limit is the configured limit for limit errors.
	Limit int64
	// Actual is the value that exceeded a limit.
	Actual int64
	// detail holds kind-specific non-sensitive rendering data, such as an
	// inclusion cycle chain.
	detail string
	// err is the underlying operating system error, if any.
	err error
}

func (e *ResponseFileError) Error() string {
	at := ""
	if loc := e.Source.String(); loc != "" {
		at = " (" + loc + ")"
	}
	switch e.Kind {
	case ResponseFileErrorNotFound, ResponseFileErrorNotReadable:
		return fmt.Sprintf("unable to read response file %q: %v%s", e.Path, e.err, at)
	case ResponseFileErrorRecursive:
		return fmt.Sprintf("response file %q references itself%s", e.Path, at)
	case ResponseFileErrorCircular:
		return fmt.Sprintf("response file inclusion cycle detected: %s%s", e.detail, at)
	case ResponseFileErrorDepth:
		return fmt.Sprintf("response file inclusion depth %d exceeds limit of %d%s", e.Actual, e.Limit, at)
	case ResponseFileErrorSize:
		return fmt.Sprintf("response file size %d bytes exceeds limit of %d bytes%s", e.Actual, e.Limit, at)
	case ResponseFileErrorArgsLimit:
		return fmt.Sprintf("expanded argument count %d exceeds limit of %d%s", e.Actual, e.Limit, at)
	case ResponseFileErrorEncoding:
		return fmt.Sprintf("response file %q contains invalid UTF-8 at %s", e.Path, e.Source.String())
	case ResponseFileErrorSyntax:
		return fmt.Sprintf("response file %q: %s%s", e.Path, e.detail, at)
	case ResponseFileErrorEmptyReference:
		return fmt.Sprintf("empty response file reference%s", at)
	case ResponseFileErrorReferenceNotAllowed:
		return fmt.Sprintf(
			"response file reference is not allowed in the value of flag %q; pass it as a separate argument instead%s",
			e.Flag, at,
		)
	default:
		return fmt.Sprintf("response file error%s", at)
	}
}

// Unwrap returns the underlying operating system error, if any, so that
// errors.Is(err, os.ErrNotExist) keeps working.
func (e *ResponseFileError) Unwrap() error {
	return e.err
}

// ExpandResponseFiles expands response file references in args using cfg
// and returns the resulting arguments together with their sources. The
// first element of args is treated as the program name and is preserved
// without expansion, matching the os.Args convention. When cfg.Enabled is
// false, args are returned unchanged apart from source attribution and no
// files are read.
//
// Relative file names are resolved against the directory of the response
// file that references them, or against the current working directory for
// references on the command line.
func ExpandResponseFiles(args []string, cfg ResponseFileConfig) ([]ExpandedArg, error) {
	seeds := make([]ExpandedArg, len(args))
	for i, arg := range args {
		seeds[i] = ExpandedArg{
			Value:  arg,
			Source: ArgSource{Column: i + 1},
		}
	}
	return expandResponseFileSeeds(seeds, cfg)
}

// expandResponseFileSeeds is the internal entry point. It accepts seeds
// with sources so that arguments read from standard input can be expanded
// as well.
func expandResponseFileSeeds(seeds []ExpandedArg, cfg ResponseFileConfig) ([]ExpandedArg, error) {
	if !cfg.Enabled {
		return seeds, nil
	}

	cfg, err := normalizeResponseFileConfig(cfg)
	if err != nil {
		return nil, err
	}

	prefix, _ := utf8.DecodeRuneInString(cfg.Prefix)
	expander := &responseFileExpander{
		cfg:    cfg,
		prefix: prefix,
	}

	out := make([]ExpandedArg, 0, len(seeds))
	for i, seed := range seeds {
		if err := expander.expandSeed(seed, i == 0, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func normalizeResponseFileConfig(cfg ResponseFileConfig) (ResponseFileConfig, error) {
	if cfg.Prefix == "" {
		cfg.Prefix = defaultResponseFilePrefix
	}
	if utf8.RuneCountInString(cfg.Prefix) != 1 {
		return cfg, fmt.Errorf("response file config: Prefix must be a single rune, got %q", cfg.Prefix)
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = defaultResponseFileDepth
	}
	if cfg.MaxFileSize <= 0 {
		cfg.MaxFileSize = defaultResponseFileSize
	}
	if cfg.MaxTotalSize <= 0 {
		cfg.MaxTotalSize = defaultResponseFileTotalSize
	}
	if cfg.MaxExpandedArgs <= 0 {
		cfg.MaxExpandedArgs = defaultResponseFileArgCount
	}
	return cfg, nil
}

type responseFileExpander struct {
	cfg       ResponseFileConfig
	prefix    rune
	openFiles []*openedResponseFile
	totalSize int64
	argCount  int
}

type openedResponseFile struct {
	key  string
	path string
	dir  string
}

// expandSeed handles one command-line-like atomic argument (as opposed to
// a word lexed from a response file, which may contain quotes).
func (e *responseFileExpander) expandSeed(seed ExpandedArg, isProgramName bool, out *[]ExpandedArg) error {
	if isProgramName {
		*out = append(*out, seed)
		return nil
	}

	if flag, atByte, blocked, doubled := e.flagValueReference(seed.Value); blocked {
		return &ResponseFileError{
			Kind:   ResponseFileErrorReferenceNotAllowed,
			Source: seed.Source,
			Flag:   flag,
		}
	} else if doubled {
		prefixLen := utf8.RuneLen(e.prefix)
		seed.Value = seed.Value[:atByte] + seed.Value[atByte+prefixLen:]
		return e.emit(seed.Value, seed.Source, out)
	}

	r, size := utf8.DecodeRuneInString(seed.Value)
	if r != e.prefix {
		return e.emit(seed.Value, seed.Source, out)
	}
	if r2, _ := utf8.DecodeRuneInString(seed.Value[size:]); r2 == e.prefix {
		// Doubled prefix introduces a literal argument.
		return e.emit(seed.Value[size:], seed.Source, out)
	}

	path := seed.Value[size:]
	if path == "" {
		return &ResponseFileError{
			Kind:   ResponseFileErrorEmptyReference,
			Source: seed.Source,
		}
	}
	return e.includeFile(path, seed.Source, out)
}

// flagValueReference inspects an atomic argument with the shape
// "--flag=@reference": it reports a bare, unescaped reference immediately
// after the first "=" as blocked, and a doubled prefix there as a literal
// value (doubled) together with the byte offset of the "@" to remove.
func (e *responseFileExpander) flagValueReference(arg string) (flag string, atByte int, blocked, doubled bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", 0, false, false
	}
	eq := strings.IndexByte(arg, '=')
	if eq < 0 {
		return "", 0, false, false
	}
	value := arg[eq+1:]
	r, size := utf8.DecodeRuneInString(value)
	if r != e.prefix {
		return "", 0, false, false
	}
	if r2, _ := utf8.DecodeRuneInString(value[size:]); r2 == e.prefix {
		return "", eq + 1, false, true
	}
	return arg[:eq], eq + 1, true, false
}

func (e *responseFileExpander) emit(value string, src ArgSource, out *[]ExpandedArg) error {
	if e.argCount >= e.cfg.MaxExpandedArgs {
		return &ResponseFileError{
			Kind:   ResponseFileErrorArgsLimit,
			Source: src,
			Limit:  int64(e.cfg.MaxExpandedArgs),
			Actual: int64(e.argCount + 1),
		}
	}
	e.argCount++
	*out = append(*out, ExpandedArg{Value: value, Source: src})
	return nil
}

func (e *responseFileExpander) includeFile(ref string, refSource ArgSource, out *[]ExpandedArg) error {
	path := ref
	if parent := e.currentFile(); parent != nil {
		if !filepath.IsAbs(ref) && parent.dir != "" {
			path = filepath.Join(parent.dir, ref)
		}
	}
	path = filepath.Clean(path)

	key, err := filepath.Abs(path)
	if err != nil {
		key = path
	}
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = filepath.Clean(resolved)
	}

	// A file already on the active inclusion stack is a recursion.
	for i, open := range e.openFiles {
		if open.key != key {
			continue
		}
		if i == len(e.openFiles)-1 {
			return &ResponseFileError{
				Kind:   ResponseFileErrorRecursive,
				Source: refSource,
				Path:   path,
			}
		}
		chain := make([]string, 0, len(e.openFiles)-i+1)
		for _, f := range e.openFiles[i:] {
			chain = append(chain, f.path)
		}
		chain = append(chain, path)
		return &ResponseFileError{
			Kind:   ResponseFileErrorCircular,
			Source: refSource,
			detail: strings.Join(chain, " -> "),
		}
	}

	depth := len(e.openFiles) + 1
	if depth > e.cfg.MaxDepth {
		return &ResponseFileError{
			Kind:   ResponseFileErrorDepth,
			Source: refSource,
			Limit:  int64(e.cfg.MaxDepth),
			Actual: int64(depth),
		}
	}

	file, err := os.Open(path)
	if err != nil {
		return &ResponseFileError{
			Kind:   ResponseFileErrorNotFound,
			Source: refSource,
			Path:   path,
			err:    err,
		}
	}

	info, statErr := file.Stat()
	if statErr == nil && info.IsDir() {
		_ = file.Close()
		return &ResponseFileError{
			Kind:   ResponseFileErrorNotReadable,
			Source: refSource,
			Path:   path,
			err:    errors.New("is a directory"),
		}
	}
	if statErr == nil && info.Size() > e.cfg.MaxFileSize {
		_ = file.Close()
		return &ResponseFileError{
			Kind:   ResponseFileErrorSize,
			Source: refSource,
			Path:   path,
			Limit:  e.cfg.MaxFileSize,
			Actual: info.Size(),
		}
	}

	// One extra byte lets us detect files that exceed the limit even when
	// the reported size is unknown or unreliable.
	data, readErr := io.ReadAll(io.LimitReader(file, e.cfg.MaxFileSize+1))
	_ = file.Close()
	if readErr != nil {
		return &ResponseFileError{
			Kind:   ResponseFileErrorNotReadable,
			Source: refSource,
			Path:   path,
			err:    readErr,
		}
	}
	if int64(len(data)) > e.cfg.MaxFileSize {
		return &ResponseFileError{
			Kind:   ResponseFileErrorSize,
			Source: refSource,
			Path:   path,
			Limit:  e.cfg.MaxFileSize,
			Actual: int64(len(data)),
		}
	}
	if e.totalSize+int64(len(data)) > e.cfg.MaxTotalSize {
		return &ResponseFileError{
			Kind:   ResponseFileErrorSize,
			Source: refSource,
			Path:   path,
			Limit:  e.cfg.MaxTotalSize,
			Actual: e.totalSize + int64(len(data)),
		}
	}
	e.totalSize += int64(len(data))

	tokens, scanErr := scanResponseFile(path, data, e.prefix)
	if scanErr != nil {
		// Scanner errors point into the file itself; record the reference
		// that brought it in as the first parent in the chain.
		scanErr.Source.IncludedFrom = &refSource
		return scanErr
	}

	opened := &openedResponseFile{
		key:  key,
		path: path,
		dir:  filepath.Dir(path),
	}
	e.openFiles = append(e.openFiles, opened)
	defer func() {
		e.openFiles = e.openFiles[:len(e.openFiles)-1]
	}()

	for _, token := range tokens {
		src := ArgSource{
			File:         path,
			Line:         token.start.line,
			Column:       token.start.col,
			EndColumn:    token.end.col,
			IncludedFrom: &refSource,
		}
		if token.isRef {
			if token.value == "" {
				return &ResponseFileError{
					Kind:   ResponseFileErrorEmptyReference,
					Source: src,
				}
			}
			if err := e.includeFile(token.value, src, out); err != nil {
				return err
			}
			continue
		}
		if err := e.emit(token.value, src, out); err != nil {
			return err
		}
	}
	return nil
}

func (e *responseFileExpander) currentFile() *openedResponseFile {
	if len(e.openFiles) == 0 {
		return nil
	}
	return e.openFiles[len(e.openFiles)-1]
}

// responseFileTraceValue replaces argument values in URFAVE_CLI_TRACING
// output for runs that expanded response files, so their contents never
// reach the trace diagnostic stream.
const responseFileTraceValue = "<response file argument: value hidden>"

// traceUsesResponseFiles reports whether the current run expanded
// response files.
func (cmd *Command) traceUsesResponseFiles() bool {
	return isTracingOn && cmd.Root().responseFileArgs != nil
}

// traceValue masks a single argument value for opt-in tracing.
func (cmd *Command) traceValue(value string) string {
	if cmd.traceUsesResponseFiles() {
		return responseFileTraceValue
	}
	return value
}

// traceAny masks a typed flag value for opt-in tracing.
func (cmd *Command) traceAny(value any) any {
	if cmd.traceUsesResponseFiles() {
		return responseFileTraceValue
	}
	return value
}

// traceArgSlice masks an argument slice for opt-in tracing. The slice is
// returned as-is when tracing is off or response files were not used.
func (cmd *Command) traceArgSlice(values []string) []string {
	if !cmd.traceUsesResponseFiles() {
		return values
	}
	return []string{responseFileTraceValue}
}

// argsForTrace renders args for opt-in URFAVE_CLI_TRACING diagnostics,
// replacing arguments that came from response files with their source
// location instead of their value, so response file contents never reach
// trace output. The returned slice shares nothing with args.
func (cmd *Command) argsForTrace(args []string) []string {
	if !isTracingOn {
		return args
	}
	sources := cmd.responseFileArgs
	if len(sources) == 0 {
		return args
	}
	out := make([]string, len(args))
	for i, arg := range args {
		if i < len(sources) {
			if file := sources[i].Source.File; file != "" && file != stdinSourceFile {
				out[i] = "<argument from " + sources[i].Source.Location() + ">"
				continue
			}
		}
		out[i] = arg
	}
	return out
}

// ---------- response file scanner ----------

type rspPosition struct {
	line int
	col  int
}

type rspToken struct {
	value      string
	start, end rspPosition
	isRef      bool
}

type rspCursor struct {
	data []byte
	i    int
	line int
	col  int
}

func newRspCursor(data []byte) *rspCursor {
	// A leading UTF-8 BOM is an encoding artifact, not an argument.
	data = bytesWithoutBOM(data)
	return &rspCursor{data: data, line: 1, col: 1}
}

func bytesWithoutBOM(data []byte) []byte {
	bom := []byte{0xEF, 0xBB, 0xBF}
	if len(data) >= len(bom) && data[0] == bom[0] && data[1] == bom[1] && data[2] == bom[2] {
		return data[len(bom):]
	}
	return data
}

// peek returns the rune at the cursor without consuming it.
func (c *rspCursor) peek() (rune, int) {
	if c.i >= len(c.data) {
		return 0, 0
	}
	r, size := utf8.DecodeRune(c.data[c.i:])
	if r == utf8.RuneError && size == 1 {
		return r, size
	}
	return r, size
}

// next consumes and returns the rune at the cursor, tracking line and
// rune column.
func (c *rspCursor) next() (rune, int) {
	r, size := c.peek()
	if size == 0 {
		return 0, 0
	}
	c.i += size
	if r == '\n' {
		c.line++
		c.col = 1
	} else {
		c.col++
	}
	return r, size
}

type rspQuoteState int

const (
	rspUnquoted rspQuoteState = iota
	rspDoubleQuoted
	rspSingleQuoted
)

// scanResponseFile turns response file contents into tokens with 1-based
// line and rune-column positions. All errors are [ResponseFileError]
// values that never include file contents.
func scanResponseFile(path string, data []byte, prefix rune) ([]rspToken, *ResponseFileError) {
	cursor := newRspCursor(data)
	var tokens []rspToken

	for {
		r, _ := cursor.peek()
		if r == 0 {
			return tokens, nil
		}
		if unicode.IsSpace(r) {
			cursor.next()
			continue
		}
		if r == '#' {
			// Comment until end of line.
			for {
				r2, size := cursor.peek()
				if size == 0 || r2 == '\n' {
					break
				}
				cursor.next()
			}
			continue
		}

		start := rspPosition{line: cursor.line, col: cursor.col}
		var builder strings.Builder
		state := rspUnquoted
		quoteStart := rspPosition{}
		isRef := false
		wordRead := false
		// Tracking for the forbidden "--flag=@file" shape.
		beginsWithDash := r == '-'
		afterEquals := false
		flagName := ""

		// Consume one word.
		for {
			r, size := cursor.peek()
			if size == 0 {
				if state != rspUnquoted {
					quote := `"`
					if state == rspSingleQuoted {
						quote = `'`
					}
					return nil, rspSyntaxError(
						path, quoteStart,
						fmt.Sprintf("unterminated %s quote", quote),
					)
				}
				break
			}

			if state == rspUnquoted {
				if unicode.IsSpace(r) {
					break
				}
				if !wordRead && r == prefix {
					if r2, _ := utf8.DecodeRune(cursor.data[cursor.i+size:]); r2 == prefix {
						// Doubled prefix introduces one literal prefix rune.
						cursor.next()
						cursor.next()
						builder.WriteRune(prefix)
						wordRead = true
						continue
					}
					isRef = true
					cursor.next()
					wordRead = true
					continue
				}
				switch r {
				case '"':
					quoteStart = rspPosition{line: cursor.line, col: cursor.col}
					state = rspDoubleQuoted
					cursor.next()
					wordRead = true
					continue
				case '\'':
					quoteStart = rspPosition{line: cursor.line, col: cursor.col}
					state = rspSingleQuoted
					cursor.next()
					wordRead = true
					continue
				case '\\':
					escapePos := rspPosition{line: cursor.line, col: cursor.col}
					cursor.next()
					r2, size2 := cursor.peek()
					if size2 == 0 {
						return nil, rspSyntaxError(path, escapePos, "dangling backslash")
					}
					cursor.next()
					if r2 != '\n' {
						builder.WriteRune(r2)
					}
					afterEquals = false
					wordRead = true
					continue
				case '=':
					if beginsWithDash {
						afterEquals = true
						flagName = builder.String()
						builder.WriteRune(r)
						cursor.next()
						wordRead = true
						continue
					}
				}
				if beginsWithDash && afterEquals && r == prefix {
					if r2, _ := utf8.DecodeRune(cursor.data[cursor.i+size:]); r2 == prefix {
						// Doubled prefix after "=" is a literal prefix rune.
						cursor.next()
						cursor.next()
						builder.WriteRune(prefix)
						afterEquals = false
						wordRead = true
						continue
					}
					return nil, &ResponseFileError{
						Kind: ResponseFileErrorReferenceNotAllowed,
						Source: ArgSource{
							File:   path,
							Line:   cursor.line,
							Column: cursor.col,
						},
						Flag: flagName,
					}
				}
				if r == utf8.RuneError && size == 1 {
					return nil, rspEncodingError(path, cursor)
				}
				afterEquals = false
				builder.WriteRune(r)
				cursor.next()
				wordRead = true
				continue
			}

			if state == rspSingleQuoted {
				if r == '\'' {
					state = rspUnquoted
					cursor.next()
					continue
				}
				if r == utf8.RuneError && size == 1 {
					return nil, rspEncodingError(path, cursor)
				}
				builder.WriteRune(r)
				cursor.next()
				continue
			}

			// Double quoted: backslash only escapes backslash, quote and
			// newline; everything else (including the backslash) is kept.
			if r == '\\' {
				escapePos := rspPosition{line: cursor.line, col: cursor.col}
				cursor.next()
				r2, size2 := cursor.peek()
				switch {
				case size2 == 0:
					return nil, rspSyntaxError(path, escapePos, "dangling backslash")
				case r2 == '"' || r2 == '\\':
					cursor.next()
					builder.WriteRune(r2)
				case r2 == '\n':
					cursor.next()
				default:
					builder.WriteRune('\\')
				}
				continue
			}
			if r == '"' {
				state = rspUnquoted
				cursor.next()
				continue
			}
			if r == utf8.RuneError && size == 1 {
				return nil, rspEncodingError(path, cursor)
			}
			builder.WriteRune(r)
			cursor.next()
		}

		end := rspPosition{line: cursor.line, col: cursor.col}
		if isRef && builder.Len() == 0 {
			return nil, &ResponseFileError{
				Kind: ResponseFileErrorEmptyReference,
				Source: ArgSource{
					File:   path,
					Line:   start.line,
					Column: start.col,
				},
			}
		}
		tokens = append(tokens, rspToken{
			value: builder.String(),
			start: start,
			end:   end,
			isRef: isRef,
		})
	}
}

func rspSyntaxError(path string, pos rspPosition, detail string) *ResponseFileError {
	return &ResponseFileError{
		Kind: ResponseFileErrorSyntax,
		Source: ArgSource{
			File:   path,
			Line:   pos.line,
			Column: pos.col,
		},
		Path:   path,
		detail: detail,
	}
}

func rspEncodingError(path string, cursor *rspCursor) *ResponseFileError {
	return &ResponseFileError{
		Kind: ResponseFileErrorEncoding,
		Source: ArgSource{
			File:   path,
			Line:   cursor.line,
			Column: cursor.col,
		},
		Path: path,
	}
}
