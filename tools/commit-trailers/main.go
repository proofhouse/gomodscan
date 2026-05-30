// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

// Command commit-trailers enforces the trailer rules that the
// commitlint binary doesn't cover. See the trailers package doc for
// the full rule set. Reads the commit message from the file given by
// -message, or from standard input.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/proofhouse/gomodscan/tools/commit-trailers/trailers"
)

func main() {
	msgFile := flag.String("message", "", "path to a commit message file; reads stdin when empty")
	flag.Parse()

	if err := run(*msgFile, os.Stdin, os.Stderr); err != nil {
		os.Exit(1)
	}
}

// run reads the commit message from msgFile, or from src when msgFile
// stays empty, and writes any rule violations to errOut. Returns a
// non-nil error when the message violates one or more rules so the
// caller can set the exit code.
func run(msgFile string, src io.Reader, errOut io.Writer) error {
	data, err := readMessage(msgFile, src)
	if err != nil {
		fmt.Fprintf(errOut, "commit-trailers: %v\n", err)
		return err
	}
	if checkErr := trailers.Check(string(data)); checkErr != nil {
		fmt.Fprintf(errOut, "commit-trailers: %v\n", checkErr)
		return fmt.Errorf("commit message rules failed: %w", checkErr)
	}
	return nil
}

func readMessage(msgFile string, src io.Reader) ([]byte, error) {
	reader := src
	if msgFile != "" {
		f, err := os.Open(msgFile)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", msgFile, err)
		}
		defer f.Close()
		reader = f
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read commit message: %w", err)
	}
	return data, nil
}
