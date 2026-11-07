package main

import (
	"bufio"
	"io"
)

// desktopStop observes only the inherited private control pipe of an explicit
// --desktop child.  EOF is also a stop: if the desktop owner dies, its station
// must not remain as an orphan.  Normal command-line mode never calls this.
func desktopStop(input io.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if input == nil {
			return
		}
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			if scanner.Text() == "stop" {
				return
			}
		}
	}()
	return done
}
