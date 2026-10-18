// Package tour serves the squark tour: lessons with runnable code, compiled and run by the real
// compiler, with the program's memory map and goals that check what a run did.
//
// Lessons live in tour/lessons, one Markdown file per chapter:
//
//	# Chapter title
//	intro paragraphs
//
//	## Lesson title
//	goal: rejects "cannot return reference"   (optional: rejects "msg", prints "text" or builds)
//	mode: build                               (optional: build and show memory, don't run)
//	exit: 2                                   (optional: the exit status a run should end with)
//
//	prose, with `code`, **bold**, *em*, [links](url), - lists, > notes and ```squark snippets
//
//	```squark editor
//	the code the lesson opens with
//	```
//
//	```squark solution
//	a solution to the goal
//	```
package tour

import (
	"fmt"
	"html"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Chapter struct {
	Slug    string    `json:"slug"`
	Title   string    `json:"title"`
	Intro   string    `json:"intro"` // HTML
	Lessons []*Lesson `json:"lessons"`
}

type Lesson struct {
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Body     string `json:"body"` // HTML
	Code     string `json:"code"`
	Solution string `json:"solution,omitempty"`
	Goal     *Goal  `json:"goal,omitempty"`
	Mode     string `json:"mode"` // "run" or "build"
	Exit     int    `json:"exit"`

	// Baked results, for an exported tour that has no compiler behind it.
	Baked         *Result `json:"baked,omitempty"`
	BakedSolution *Result `json:"bakedSolution,omitempty"`
}

// A Goal is what a lesson asks for, checked against a run's result.
type Goal struct {
	Kind string `json:"kind"` // "rejects", "prints" or "builds"
	Text string `json:"text,omitempty"`
}

// Met reports whether r achieves g.
func (g *Goal) Met(r *Result) bool {
	switch g.Kind {
	case "rejects":
		if r.Stage != "compile" {
			return false
		}
		for _, d := range r.Errors {
			if strings.Contains(d.Msg, g.Text) {
				return true
			}
		}
		return strings.Contains(r.Output, g.Text)
	case "prints":
		return r.Stage == "run" && r.Exit == 0 && !r.TimedOut && strings.Contains(r.Output, g.Text)
	case "builds":
		return r.Stage == "run" || r.Stage == "built"
	}
	return false
}

// Load reads every chapter in dir of fsys, in file name order.
func Load(fsys fs.FS, dir string) ([]*Chapter, error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var chs []*Chapter
	seen := map[string]bool{}
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		slug := strings.TrimSuffix(path.Base(f), ".md")
		if i := strings.IndexByte(slug, '-'); i >= 0 {
			slug = slug[i+1:] // 03-arenas.md: arenas
		}
		ch, err := parseChapter(slug, string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		for _, l := range ch.Lessons {
			id := ch.Slug + "/" + l.Slug
			if seen[id] {
				return nil, fmt.Errorf("%s: two lessons are named %s", f, id)
			}
			seen[id] = true
		}
		chs = append(chs, ch)
	}
	return chs, nil
}

var goalRE = regexp.MustCompile(`^goal:\s*(rejects|prints)\s+"((?:[^"\\]|\\.)*)"\s*$|^goal:\s*(builds)\s*$`)

func parseChapter(slug, src string) (*Chapter, error) {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "# ") {
		return nil, fmt.Errorf("a chapter starts with '# Title'")
	}
	ch := &Chapter{Slug: slug, Title: strings.TrimSpace(lines[0][2:])}
	var cur *Lesson
	var body []string
	flush := func() error {
		if cur == nil {
			ch.Intro = renderMarkdown(body)
			return nil
		}
		if err := finishLesson(cur, body); err != nil {
			return fmt.Errorf("lesson %q: %v", cur.Title, err)
		}
		ch.Lessons = append(ch.Lessons, cur)
		return nil
	}
	inFence := false
	for _, ln := range lines[1:] {
		if strings.HasPrefix(ln, "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(ln, "## ") {
			if err := flush(); err != nil {
				return nil, err
			}
			title := strings.TrimSpace(ln[3:])
			cur = &Lesson{Title: title, Slug: slugify(title), Mode: "run"}
			body = nil
			continue
		}
		body = append(body, ln)
	}
	if inFence {
		return nil, fmt.Errorf("unclosed ``` block")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(ch.Lessons) == 0 {
		return nil, fmt.Errorf("no lessons (## Title)")
	}
	return ch, nil
}

func finishLesson(l *Lesson, lines []string) error {
	// header lines come first, before the prose
	i := 0
header:
	for ; i < len(lines); i++ {
		ln := strings.TrimSpace(lines[i])
		if ln == "" {
			if i == 0 {
				continue
			}
			break
		}
		switch {
		case strings.HasPrefix(ln, "goal:"):
			m := goalRE.FindStringSubmatch(ln)
			if m == nil {
				return fmt.Errorf(`bad goal %q: want rejects "msg", prints "text" or builds`, ln)
			}
			if m[3] != "" {
				l.Goal = &Goal{Kind: m[3]}
			} else {
				t, err := strconv.Unquote(`"` + m[2] + `"`)
				if err != nil {
					return fmt.Errorf("bad goal %q: %v", ln, err)
				}
				l.Goal = &Goal{Kind: m[1], Text: t}
			}
		case strings.HasPrefix(ln, "mode:"):
			l.Mode = strings.TrimSpace(ln[5:])
			if l.Mode != "run" && l.Mode != "build" {
				return fmt.Errorf("mode is run or build, not %q", l.Mode)
			}
		case strings.HasPrefix(ln, "exit:"):
			n, err := strconv.Atoi(strings.TrimSpace(ln[5:]))
			if err != nil {
				return fmt.Errorf("bad exit: %v", err)
			}
			l.Exit = n
		default:
			break header // the prose starts here
		}
	}
	var prose []string
	for ; i < len(lines); i++ {
		ln := lines[i]
		if ln == "```squark editor" || ln == "```squark solution" {
			var code []string
			for i++; i < len(lines) && lines[i] != "```"; i++ {
				code = append(code, lines[i])
			}
			src := strings.Join(code, "\n") + "\n"
			if strings.HasSuffix(ln, "editor") {
				if l.Code != "" {
					return fmt.Errorf("two editor blocks")
				}
				l.Code = src
			} else {
				l.Solution = src
			}
			continue
		}
		prose = append(prose, ln)
	}
	if l.Code == "" {
		return fmt.Errorf("no ```squark editor block")
	}
	if l.Goal != nil && l.Solution == "" {
		return fmt.Errorf("a lesson with a goal needs a ```squark solution block")
	}
	l.Body = renderMarkdown(prose)
	return nil
}

func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// renderMarkdown renders the small subset of Markdown the lessons use.
func renderMarkdown(lines []string) string {
	var b strings.Builder
	var para []string
	endPara := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + inline(strings.Join(para, " ")) + "</p>\n")
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		t := strings.TrimSpace(ln)
		switch {
		case t == "":
			endPara()
		case strings.HasPrefix(t, "```"):
			endPara()
			lang := strings.TrimSpace(t[3:])
			var code []string
			for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
				code = append(code, lines[i])
			}
			fmt.Fprintf(&b, "<pre class=\"snippet\" data-lang=\"%s\"><code>%s</code></pre>\n", html.EscapeString(lang), html.EscapeString(strings.Join(code, "\n")))
		case strings.HasPrefix(t, "### "):
			endPara()
			b.WriteString("<h3>" + inline(t[4:]) + "</h3>\n")
		case strings.HasPrefix(t, "- "):
			endPara()
			b.WriteString("<ul>\n")
			for ; i < len(lines); i++ {
				t := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(t, "- ") {
					i--
					break
				}
				item := t[2:]
				// continuation lines are indented
				for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ") && strings.TrimSpace(lines[i+1]) != "" && !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "- ") {
					i++
					item += " " + strings.TrimSpace(lines[i])
				}
				b.WriteString("<li>" + inline(item) + "</li>\n")
			}
			b.WriteString("</ul>\n")
		case strings.HasPrefix(t, "|"):
			endPara()
			b.WriteString("<table>\n")
			row := 0
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				cells := strings.Split(strings.Trim(strings.TrimSpace(lines[i]), "|"), "|")
				if row == 1 && strings.Trim(strings.Join(cells, ""), "-: ") == "" {
					row++
					continue // the |---| line under the header
				}
				tag := "td"
				if row == 0 {
					tag = "th"
				}
				b.WriteString("<tr>")
				for _, c := range cells {
					fmt.Fprintf(&b, "<%s>%s</%s>", tag, inline(strings.TrimSpace(c)), tag)
				}
				b.WriteString("</tr>\n")
				row++
			}
			i--
			b.WriteString("</table>\n")
		case strings.HasPrefix(t, ">"):
			endPara()
			var q []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				q = append(q, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
			}
			i--
			b.WriteString("<aside>" + inline(strings.Join(q, " ")) + "</aside>\n")
		default:
			para = append(para, t)
		}
	}
	endPara()
	return b.String()
}

var (
	codeRE  = regexp.MustCompile("`([^`]+)`")
	boldRE  = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	emRE    = regexp.MustCompile(`\*([^*\s][^*]*)\*`)
	linkRE  = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	kbdRE   = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	placeRE = regexp.MustCompile("\x00(\\d+)\x00")
)

// inline renders `code`, **bold**, *em*, [[key]] and [text](url). Code spans are taken out
// first so that nothing inside them is formatted.
func inline(s string) string {
	var codes []string
	s = codeRE.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, "<code>"+html.EscapeString(m[1:len(m)-1])+"</code>")
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	})
	s = html.EscapeString(s)
	s = linkRE.ReplaceAllStringFunc(s, func(m string) string {
		p := linkRE.FindStringSubmatch(m)
		attr := ""
		if strings.HasPrefix(p[2], "http") {
			attr = ` target="_blank" rel="noopener"`
		}
		return fmt.Sprintf(`<a href="%s"%s>%s</a>`, p[2], attr, p[1])
	})
	s = kbdRE.ReplaceAllString(s, "<kbd>$1</kbd>")
	s = boldRE.ReplaceAllString(s, "<strong>$1</strong>")
	s = emRE.ReplaceAllString(s, "<em>$1</em>")
	return placeRE.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(m[1 : len(m)-1])
		return codes[n]
	})
}
