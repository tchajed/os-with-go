// verify-lines checks that code block citations in the course markdown files
// match the actual Go runtime source code.
//
// Usage:
//
//	go run ./tools/verify-lines -go-root ~/sw/go [-fix]
//
// It finds code blocks with citation comments like:
//
//	// src/runtime/proc.go, lines 24-34
//
// and verifies that the first and last non-empty lines of the code block match
// the corresponding lines in the Go source tree. When they don't match, it
// searches for the correct location and reports the fix.
//
// With -fix, it rewrites the markdown files in place (updating both the
// citation comment inside the code block and the URL in any preceding link).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	goRoot  = flag.String("go-root", "", "path to Go source tree (required)")
	srcDir  = flag.String("src", "src", "path to course markdown source directory")
	fix     = flag.Bool("fix", false, "rewrite files with corrected line numbers")
	goTag   = flag.String("tag", "", "Go version tag for URLs (e.g. go1.26.1); if empty, extracted from GO_ROOT/VERSION")
	verbose = flag.Bool("v", false, "print OK blocks too")
)

// citationRe matches citation comments inside code blocks.
// Handles formats:
//
//	// src/runtime/proc.go, lines 24-34
//	// src/runtime/proc.go lines 24-34
//	// src/runtime/proc.go, line 42
//	// src/runtime/proc.go, lines 24-34 (URL)
//	// [src/runtime/proc.go, lines 24-34](URL)
//	// src/runtime/stack.go, lines 1014-1016, 1026
var citationRe = regexp.MustCompile(
	`^// ` +
		`(?:\[)?` + // optional markdown link open
		`(src/runtime/[\w.]+)` + // group 1: file path
		`,?\s+lines?\s+` +
		`(\d+)` + // group 2: start line
		`(?:-(\d+))?` + // group 3: optional end line
		`(?:,\s+(\d+))?`, // group 4: optional extra line
)

// linkRe matches inline markdown links to source files in prose.
var linkRe = regexp.MustCompile(
	`\[` + "`?" +
		`(src/runtime/[\w.]+)` + // group 1: file path
		"`?" + `,?\s+lines?\s+` +
		`(\d+)` + // group 2: start
		`(?:-(\d+))?` + // group 3: end
		`\]` +
		`\(https://cs\.opensource\.google/go/go/\+/[^)]+\)`,
)

type citation struct {
	file       string // relative path, e.g. "src/runtime/proc.go"
	start, end int    // 1-indexed, inclusive

	// location in the markdown file
	mdFile string
	mdLine int // 1-indexed line of the citation comment

	// the code from the markdown block (excluding citation line)
	code []string
}

type result struct {
	cit          citation
	status       string // "ok", "mismatch", "not-found", "whitespace-only"
	newStart     int
	newEnd       int
	searchedFrom string // description of how we found the new location
}

func main() {
	flag.Parse()
	if *goRoot == "" {
		log.Fatal("-go-root is required")
	}

	tag := *goTag
	if tag == "" {
		data, err := os.ReadFile(filepath.Join(*goRoot, "VERSION"))
		if err != nil {
			log.Fatalf("reading VERSION from %s: %v (use -tag to set manually)", *goRoot, err)
		}
		tag = strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	}

	var mdFiles []string
	err := filepath.Walk(*srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			mdFiles = append(mdFiles, path)
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	var citations []citation
	for _, f := range mdFiles {
		cits, err := extractCitations(f)
		if err != nil {
			log.Printf("warning: %s: %v", f, err)
			continue
		}
		citations = append(citations, cits...)
	}

	fmt.Printf("Found %d code block citations across %d files\n\n", len(citations), len(mdFiles))

	// Cache source files
	srcCache := map[string][]string{}

	var results []result
	issues := 0
	for _, cit := range citations {
		r := verify(cit, srcCache)
		results = append(results, r)
		if r.status != "ok" && r.status != "whitespace-only" {
			issues++
		}
	}

	// Print results
	for _, r := range results {
		rel, _ := filepath.Rel(*srcDir, r.cit.mdFile)
		switch r.status {
		case "ok":
			if *verbose {
				fmt.Printf("  OK  %s:%d  %s lines %d-%d\n",
					rel, r.cit.mdLine, r.cit.file, r.cit.start, r.cit.end)
			}
		case "whitespace-only":
			if *verbose {
				fmt.Printf("  WS  %s:%d  %s lines %d-%d (whitespace difference only)\n",
					rel, r.cit.mdLine, r.cit.file, r.cit.start, r.cit.end)
			}
		case "mismatch":
			fmt.Printf("  FIX %s:%d  %s lines %d-%d -> %d-%d  (%s)\n",
				rel, r.cit.mdLine, r.cit.file,
				r.cit.start, r.cit.end, r.newStart, r.newEnd, r.searchedFrom)
		case "not-found":
			fmt.Printf("  ??? %s:%d  %s lines %d-%d  (could not find correct location)\n",
				rel, r.cit.mdLine, r.cit.file, r.cit.start, r.cit.end)
			if len(r.cit.code) > 0 {
				first := strings.TrimSpace(r.cit.code[0])
				if len(first) > 80 {
					first = first[:80] + "..."
				}
				fmt.Printf("       first line: %s\n", first)
			}
		}
	}

	if issues > 0 {
		fmt.Printf("\n%d issues found out of %d citations\n", issues, len(citations))
	} else {
		fmt.Printf("\nAll %d citations verified OK\n", len(citations))
	}

	if *fix && issues > 0 {
		applyFixes(results, tag)
	}
}

func extractCitations(path string) ([]citation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var citations []citation
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	inCodeBlock := false
	var codeLines []string
	var codeStart int

	for i, line := range lines {
		if strings.HasPrefix(line, "```go") || strings.HasPrefix(line, "```asm") || strings.HasPrefix(line, "```s") {
			inCodeBlock = true
			codeLines = nil
			codeStart = i + 1
			continue
		}
		if strings.HasPrefix(line, "```") && inCodeBlock {
			inCodeBlock = false
			if len(codeLines) > 0 {
				m := citationRe.FindStringSubmatch(strings.TrimSpace(codeLines[0]))
				if m != nil {
					start, _ := strconv.Atoi(m[2])
					end := start
					if m[3] != "" {
						end, _ = strconv.Atoi(m[3])
					}
					// Code is everything after the citation line
					code := codeLines[1:]
					// Skip blank line after citation
					if len(code) > 0 && strings.TrimSpace(code[0]) == "" {
						code = code[1:]
					}
					citations = append(citations, citation{
						file:   m[1],
						start:  start,
						end:    end,
						mdFile: path,
						mdLine: codeStart + 1, // 1-indexed
						code:   code,
					})
				}
			}
			continue
		}
		if inCodeBlock {
			codeLines = append(codeLines, line)
		}
	}

	return citations, nil
}

func readSourceFile(path string, cache map[string][]string) ([]string, error) {
	if lines, ok := cache[path]; ok {
		return lines, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	cache[path] = lines
	return lines, nil
}

func norm(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func verify(cit citation, cache map[string][]string) result {
	srcPath := filepath.Join(*goRoot, cit.file)
	srcLines, err := readSourceFile(srcPath, cache)
	if err != nil {
		return result{cit: cit, status: "not-found", searchedFrom: "file not found"}
	}

	code := nonEmpty(cit.code)
	if len(code) == 0 {
		return result{cit: cit, status: "ok"}
	}

	// Check if current line numbers match
	if cit.end <= len(srcLines) {
		src := nonEmpty(srcLines[cit.start-1 : cit.end])
		if len(src) > 0 {
			firstMatch := norm(code[0]) == norm(src[0])
			lastMatch := norm(code[len(code)-1]) == norm(src[len(src)-1])
			if firstMatch && lastMatch {
				// Check if it's exact or whitespace-only difference
				if code[0] == strings.TrimRight(srcLines[cit.start-1], "\n\r") {
					return result{cit: cit, status: "ok"}
				}
				return result{cit: cit, status: "whitespace-only"}
			}
		}
	}

	// Lines don't match -- search for correct location
	firstTarget := norm(code[0])
	lastTarget := norm(code[len(code)-1])
	span := cit.end - cit.start

	// Find all occurrences of the first line
	var firstMatches []int
	for i, line := range srcLines {
		if norm(line) == firstTarget {
			firstMatches = append(firstMatches, i+1) // 1-indexed
		}
	}

	if len(firstMatches) == 0 {
		return result{cit: cit, status: "not-found", searchedFrom: "first line not found in source"}
	}

	// For each first-line match, search nearby for the last line
	for _, fm := range firstMatches {
		for offset := -30; offset <= 30; offset++ {
			checkEnd := fm + span + offset
			if checkEnd < 1 || checkEnd > len(srcLines) || checkEnd < fm {
				continue
			}
			if norm(srcLines[checkEnd-1]) == lastTarget {
				return result{
					cit:          cit,
					status:       "mismatch",
					newStart:     fm,
					newEnd:       checkEnd,
					searchedFrom: fmt.Sprintf("first line at %d", fm),
				}
			}
		}
	}

	// Couldn't find matching last line -- just report the first match
	return result{
		cit:          cit,
		status:       "not-found",
		searchedFrom: fmt.Sprintf("first line found at %v but last line not found nearby", firstMatches),
	}
}

func nonEmpty(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func applyFixes(results []result, tag string) {
	// Group fixes by file
	byFile := map[string][]result{}
	for _, r := range results {
		if r.status == "mismatch" {
			byFile[r.cit.mdFile] = append(byFile[r.cit.mdFile], r)
		}
	}

	for path, fixes := range byFile {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("error reading %s: %v", path, err)
			continue
		}
		content := string(data)

		for _, r := range fixes {
			content = fixCitation(content, r, tag)
		}

		if content != string(data) {
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				log.Printf("error writing %s: %v", path, err)
			} else {
				rel, _ := filepath.Rel(*srcDir, path)
				fmt.Printf("  wrote %s\n", rel)
			}
		}
	}
}

func fixCitation(content string, r result, tag string) string {
	oldStart := strconv.Itoa(r.cit.start)
	oldEnd := strconv.Itoa(r.cit.end)
	newStart := strconv.Itoa(r.newStart)
	newEnd := strconv.Itoa(r.newEnd)

	baseURL := fmt.Sprintf("https://cs.opensource.google/go/go/+/refs/tags/%s:", tag)

	// Fix citation comment patterns (multiple formats)
	oldRange := oldStart + "-" + oldEnd
	newRange := newStart + "-" + newEnd

	// Replace in citation comments: "FILE, lines OLD" -> "FILE, lines NEW"
	for _, sep := range []string{", lines ", " lines ", ",lines "} {
		old := r.cit.file + sep + oldRange
		new := r.cit.file + sep + newRange
		content = strings.ReplaceAll(content, old, new)
	}

	// Also fix "lines OLD" standalone (for single-line citations)
	if r.cit.start == r.cit.end {
		for _, sep := range []string{", line ", " line "} {
			old := r.cit.file + sep + oldStart
			new := r.cit.file + sep + newStart
			content = strings.ReplaceAll(content, old, new)
		}
	}

	// Fix URLs: update ;l=OLD_START to ;l=NEW_START
	oldURL := baseURL + r.cit.file + ";l=" + oldStart
	newURL := baseURL + r.cit.file + ";l=" + newStart
	content = strings.ReplaceAll(content, oldURL, newURL)

	return content
}
