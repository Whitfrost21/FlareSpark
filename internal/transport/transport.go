// Package transport handles newline-delimited JSON over stdio.
package transport

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

var (
	stdout  *bufio.Writer
	writeMu sync.Mutex
)

func Init() {
	stdout = bufio.NewWriter(os.Stdout)
}

// Write serializes v as JSON and writes it as a single newline-terminated line.
func Write(v any) {
	data, _ := json.Marshal(v)
	writeMu.Lock()
	defer writeMu.Unlock()
	stdout.Write(data)
	stdout.WriteByte('\n')
	stdout.Flush()
}

// Scanner returns a line scanner over stdin with a 1 MB buffer.
func Scanner() *bufio.Scanner {
	s := bufio.NewScanner(os.Stdin)
	s.Buffer(make([]byte, 1024*1024), 1024*1024)
	return s
}
