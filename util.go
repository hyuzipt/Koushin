package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

func logPath() string {
	return filepath.Join(appDataDir(), "koushin.log")
}

func logAppend(lines ...string) {
	f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, s := range lines {
		ts := time.Now().Format("2006-01-02 15:04:05")
		_, _ = f.WriteString(ts + " " + s + "\n")
	}
}

func logRecoveredPanic(where string) {
	if r := recover(); r != nil {
		logAppend("panic in "+where+": ", fmt.Sprint(r))
		logAppend(string(debug.Stack()))
	}
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
