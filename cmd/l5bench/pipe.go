package main

import "io"

type pipeReader = io.PipeReader
type pipeWriter = io.PipeWriter

func newPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }
