package classify

import "strings"

// forEachSegment calls fn with each simple-command segment of a shell command.
//
// It is a scanner rather than a parser, and the three things it handles are the three that
// were measured to matter on the real corpus:
//
//	quotes         `echo "a && b"` is one command, not two
//	heredocs       `python3 - <<'PY' ... PY` — the body is DATA
//	substitutions  `$(git rev-list --count HEAD)` contains a command
//
// The heredoc case is not a nicety. `python3 - <<'PY'` is one of the commonest shapes in this
// corpus, and without stripping the body the Python source inside is scanned as a pile of
// shell commands named `import`, `return` and `PY`. That defect was visible in the first
// measurement as a binary called `1` ranking above `go`.
func forEachSegment(cmd string, fn func(string)) {
	cmd = stripHeredocs(cmd)
	var sb strings.Builder
	flush := func() {
		if sb.Len() > 0 {
			fn(sb.String())
			sb.Reset()
		}
	}
	for i := 0; i < len(cmd); {
		c := cmd[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				sb.WriteString(cmd[i:])
				i = len(cmd)
				continue
			}
			sb.WriteString(cmd[i : i+1+j+1])
			i += 1 + j + 1
		case c == '"':
			j := i + 1
			for j < len(cmd) {
				if cmd[j] == '\\' {
					j += 2
					continue
				}
				if cmd[j] == '"' {
					break
				}
				j++
			}
			if j >= len(cmd) {
				sb.WriteString(cmd[i:])
				i = len(cmd)
				continue
			}
			sb.WriteString(cmd[i : j+1])
			i = j + 1
		case c == '\\' && i+1 < len(cmd):
			i += 2
		case c == '`':
			j := strings.IndexByte(cmd[i+1:], '`')
			if j < 0 {
				i = len(cmd)
				continue
			}
			flush()
			forEachSegment(cmd[i+1:i+1+j], fn)
			i += 1 + j + 1
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			depth, j := 1, i+2
			for j < len(cmd) && depth > 0 {
				if cmd[j] == '(' {
					depth++
				} else if cmd[j] == ')' {
					depth--
				}
				j++
			}
			flush()
			end := j - 1
			if end > len(cmd) {
				end = len(cmd)
			}
			if i+2 <= end {
				forEachSegment(cmd[i+2:end], fn)
			}
			i = j
		case c == '&' && i+1 < len(cmd) && cmd[i+1] == '&',
			c == '|' && i+1 < len(cmd) && cmd[i+1] == '|':
			flush()
			i += 2
		case c == '&' && sb.Len() > 0 && sb.String()[sb.Len()-1] == '>':
			// `2>&1` and `&>file` are redirections, not the background operator.
			sb.WriteByte(c)
			i++
		case c == '|' || c == ';' || c == '\n' || c == '&':
			flush()
			i++
		default:
			sb.WriteByte(c)
			i++
		}
	}
	flush()
}

// stripHeredocs removes heredoc bodies, which are data rather than commands.
func stripHeredocs(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); {
		line := lines[i]
		out = append(out, line)
		tags := heredocTags(line)
		i++
		for _, tag := range tags {
			for i < len(lines) && strings.TrimSpace(lines[i]) != tag {
				i++
			}
			i++ // skip the terminator
		}
	}
	return strings.Join(out, "\n")
}

// heredocTags returns the terminators opened on one line: `<<EOF`, `<<-EOF`, `<<'EOF'`, `<<"EOF"`.
func heredocTags(line string) []string {
	var tags []string
	for i := 0; i+2 < len(line); i++ {
		if line[i] != '<' || line[i+1] != '<' {
			continue
		}
		j := i + 2
		if j < len(line) && line[j] == '<' {
			continue // `<<<` is a here-string: one line, no body
		}
		if j < len(line) && line[j] == '-' {
			j++
		}
		for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
			j++
		}
		var quote byte
		if j < len(line) && (line[j] == '\'' || line[j] == '"') {
			quote = line[j]
			j++
		}
		start := j
		for j < len(line) && isTagByte(line[j]) {
			j++
		}
		if j == start {
			continue
		}
		tag := line[start:j]
		if quote != 0 {
			if j >= len(line) || line[j] != quote {
				continue
			}
			j++
		}
		tags = append(tags, tag)
		i = j - 1
	}
	return tags
}

func isTagByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// fields splits a segment into words, stripping one level of quoting.
func fields(seg string) []string {
	var out []string
	var sb strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			out = append(out, sb.String())
			sb.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(seg); {
		c := seg[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++
		case c == '\'':
			j := strings.IndexByte(seg[i+1:], '\'')
			if j < 0 {
				sb.WriteString(seg[i+1:])
				inWord = true
				i = len(seg)
				continue
			}
			sb.WriteString(seg[i+1 : i+1+j])
			inWord = true
			i += 1 + j + 1
		case c == '"':
			j := i + 1
			for j < len(seg) {
				if seg[j] == '\\' && j+1 < len(seg) {
					sb.WriteByte(seg[j+1])
					j += 2
					continue
				}
				if seg[j] == '"' {
					break
				}
				sb.WriteByte(seg[j])
				j++
			}
			inWord = true
			i = j + 1
		default:
			sb.WriteByte(c)
			inWord = true
			i++
		}
	}
	flush()
	return out
}
