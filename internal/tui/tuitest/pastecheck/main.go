// Command pastecheck is a test stand-in for an agent's input box with paste
// detection, the way Claude and Codex judge it: in raw mode, an Enter that arrives in
// the same read as text, or less than a threshold after the last text, is part of a
// paste and becomes a newline; only an Enter on its own submits. Each submit prints
// SUBMIT[<text>] (newlines as ⏎) and appends the text to a log file. It turns on
// bracketed paste, like those agents do.
//
// usage: pastecheck <threshold> <log>
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: pastecheck <threshold> <log>")
		os.Exit(2)
	}
	threshold, err := time.ParseDuration(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	stty := exec.Command("/bin/stty", "raw", "-echo")
	stty.Stdin = os.Stdin
	if err := stty.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "stty:", err)
		os.Exit(1)
	}
	fmt.Print("\x1b[?2004h") // bracketed paste on
	fmt.Print("pastecheck ready\r\n> ")
	var line strings.Builder
	var last time.Time // the last text byte
	inPaste := false
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		now := time.Now()
		chunk := string(buf[:n])
		textBefore := false // this read had text before the byte at hand
		for len(chunk) > 0 {
			switch {
			case strings.HasPrefix(chunk, "\x1b[200~"):
				inPaste, chunk = true, chunk[6:]
				continue
			case strings.HasPrefix(chunk, "\x1b[201~"):
				inPaste, chunk = false, chunk[6:]
				last, textBefore = now, true
				continue
			}
			c := chunk[0]
			chunk = chunk[1:]
			switch {
			case c == '\r' || c == '\n':
				if inPaste || textBefore || now.Sub(last) < threshold {
					line.WriteString("\n") // part of a paste
					last = now
					continue
				}
				text := line.String()
				line.Reset()
				fmt.Printf("\r\nSUBMIT[%s]\r\n> ", strings.ReplaceAll(text, "\n", "⏎"))
				if f, err := os.OpenFile(os.Args[2], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
					_, _ = fmt.Fprintf(f, "%s\n", strings.ReplaceAll(text, "\n", "⏎"))
					_ = f.Close()
				}
			case c == 0x7f:
				if s := []rune(line.String()); len(s) > 0 {
					line.Reset()
					line.WriteString(string(s[:len(s)-1]))
				}
			case c == 0x1b || c < 0x20:
				// other keys: ignored
			default:
				line.WriteByte(c)
				last, textBefore = now, true
			}
		}
		fmt.Printf("\r\x1b[K> %s", strings.ReplaceAll(line.String(), "\n", "⏎"))
	}
}
