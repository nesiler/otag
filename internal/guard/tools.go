package guard

import (
	"encoding/json"
	"regexp"
	"strings"
)

// pathKey matches argument names that name a file or directory in a tool's input.
var pathKey = regexp.MustCompile(`(?i)^(path|paths|file|files|filename|filenames|file_?path|file_?paths|` +
	`notebook_?path|target|target_?path|destination|dest|dest_?path|output|output_?path|out|dir|directory|` +
	`cwd|workdir|working_?directory|source|src|uri)$`)

// commandKey matches argument names that carry a shell command.
var commandKey = regexp.MustCompile(`(?i)^(command|commands|cmd|script|shell|shell_?command)$`)

// execTool matches tool names that run commands (exec, shell, terminal, ...).
var execTool = regexp.MustCompile(`(?i)(exec|shell|terminal|bash|command|run_?code|eval|script|spawn|process)`)

// writeTool matches tool names that write files (write, edit, create, move, ...); a
// light run checks the path arguments of only these as writes.
var writeTool = regexp.MustCompile(`(?i)(write|edit|create|delete|remove|move|rename|patch|save|put|upload|mkdir|copy|replace|insert|append)`)

// CheckToolArgs checks a tool otag does not know (an MCP server's, say; MCP servers run
// outside the harness's sandbox) by its name and input, which must be a JSON object:
//   - every path-like argument (path, file_path, destination, ...) must pass
//     CheckWrite, since otag cannot tell whether the tool writes;
//   - a command-like argument (command, cmd, script, shell) must be a string (or a
//     list of strings, an argv) and pass CheckCommand;
//   - a tool whose name says it runs commands but whose input has no command-like
//     string to check is refused.
//
// A light run has no executor heuristics: input it cannot read is allowed, a string
// command is checked like a shell line, and path arguments are checked as writes only
// for a tool named like a writer (writeTool), otherwise as reads (deny-glob files).
func (p Policy) CheckToolArgs(tool string, input []byte) Decision {
	if p.Workdir == "" {
		return allow
	}
	light := p.Level.IsLight()
	var m map[string]any
	if s := strings.TrimSpace(string(input)); s != "" && s != "null" {
		if err := json.Unmarshal(input, &m); err != nil {
			if light {
				return allow
			}
			return deny("cannot inspect the arguments of %s (not a JSON object); refusing it", tool)
		}
	}
	checked := false
	d := p.walkArgs(m, 0, &checked, !light || writeTool.MatchString(tool))
	if d.Allow && !checked && execTool.MatchString(tool) && !light {
		return deny("%s looks like it runs commands, and otag cannot see which; refusing it", tool)
	}
	return d
}

// walkArgs checks the command- and path-like arguments in v; writes says whether
// paths are checked as writes (else as reads).
func (p Policy) walkArgs(v any, depth int, checked *bool, writes bool) Decision {
	if depth > 6 {
		return allow
	}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if commandKey.MatchString(k) {
				cmd, ok := commandText(val)
				if !ok {
					if p.Level.IsLight() {
						continue
					}
					return deny("cannot inspect the tool argument %s; refusing it", k)
				}
				if d := p.CheckCommand(cmd); !d.Allow {
					return Decision{Reason: d.Reason + " (tool argument " + k + ")"}
				}
				*checked = true
				continue
			}
			if pathKey.MatchString(k) {
				for _, s := range pathStrings(val) {
					if !writes {
						if d := p.CheckRead(s); !d.Allow {
							return Decision{Reason: d.Reason + " (tool argument " + k + ")"}
						}
						continue
					}
					if d := p.CheckWrite(s); !d.Allow {
						return Decision{Reason: d.Reason + " (tool argument " + k + "; tools otag does not know may not touch paths outside the workspace)"}
					}
				}
			}
			if d := p.walkArgs(val, depth+1, checked, writes); !d.Allow {
				return d
			}
		}
	case []any:
		for _, e := range x {
			if d := p.walkArgs(e, depth+1, checked, writes); !d.Allow {

				return d
			}
		}
	}
	return allow
}

// commandText returns a command argument as a shell line: a string, or a list of
// strings taken as an argv.
func commandText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []any:
		words := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return "", false
			}
			words = append(words, s)
		}
		return joinWords(words), true
	}
	return "", false
}

// pathStrings returns the paths in a path argument (a string or a list of strings);
// URLs other than file:// are not paths.
func pathStrings(v any) []string {
	var in []string
	switch x := v.(type) {
	case string:
		in = []string{x}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				in = append(in, s)
			}
		}
	}
	var out []string
	for _, s := range in {
		if rest, ok := strings.CutPrefix(s, "file://"); ok {
			s = rest
		} else if strings.Contains(s, "://") {
			continue
		}
		if s, ok := expandHome(s); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
