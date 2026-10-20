/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rckube

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Input frames carry an attach client's input when its end must close the
// process's input. The end travels on the same connection as the data, after
// it, so no input is lost; a half-close would need Windows message-mode pipes.
// A frame is a 4-byte big-endian length and that many bytes. A zero length
// marks the end of input.
const maxInputFrame = 32 << 10

// writeInputFrames copies input as frames and then writes the end marker.
func writeInputFrames(output io.Writer, input io.Reader) error {
	buffer := make([]byte, 4+maxInputFrame)
	for {
		count, err := input.Read(buffer[4:])
		if count > 0 {
			binary.BigEndian.PutUint32(buffer[:4], uint32(count))
			if _, writeErr := output.Write(buffer[:4+count]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			binary.BigEndian.PutUint32(buffer[:4], 0)
			_, writeErr := output.Write(buffer[:4])
			return writeErr
		}
		if err != nil {
			return err
		}
	}
}

// inputFrameReader decodes frames. It returns io.EOF only at the end marker;
// a connection that ends without one is an error, not end of input.
type inputFrameReader struct {
	reader    io.Reader
	remaining uint32
	ended     bool
}

func (frames *inputFrameReader) Read(data []byte) (int, error) {
	if frames.ended {
		return 0, io.EOF
	}
	if frames.remaining == 0 {
		var header [4]byte
		if _, err := io.ReadFull(frames.reader, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, fmt.Errorf("read input frame: %w", err)
		}
		frames.remaining = binary.BigEndian.Uint32(header[:])
		if frames.remaining == 0 {
			frames.ended = true
			return 0, io.EOF
		}
		if frames.remaining > maxInputFrame {
			return 0, fmt.Errorf("input frame of %d bytes exceeds %d", frames.remaining, maxInputFrame)
		}
	}
	if uint32(len(data)) > frames.remaining {
		data = data[:frames.remaining]
	}
	count, err := frames.reader.Read(data)
	frames.remaining -= uint32(count)
	if errors.Is(err, io.EOF) {
		return count, io.ErrUnexpectedEOF
	}

	return count, err
}
