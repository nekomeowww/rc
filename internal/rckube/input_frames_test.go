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
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInputFramesRoundTrip(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 3*maxInputFrame+17)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	stream := new(bytes.Buffer)
	require.NoError(t, writeInputFrames(stream, bytes.NewReader(payload)))
	stream.WriteString("ignored after the end")

	decoded, err := io.ReadAll(&inputFrameReader{reader: stream})
	require.NoError(t, err, "the end marker is end of input")
	require.Equal(t, payload, decoded)
}

func TestInputFramesWithoutEndAreNotEndOfInput(t *testing.T) {
	t.Parallel()
	stream := new(bytes.Buffer)
	require.NoError(t, writeInputFrames(stream, bytes.NewReader([]byte("partial"))))
	stream.Truncate(stream.Len() - 4)
	_, err := io.ReadAll(&inputFrameReader{reader: stream})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF, "a dropped connection must not close the process input")

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxInputFrame+1)
	_, err = io.ReadAll(&inputFrameReader{reader: bytes.NewReader(header[:])})
	require.Error(t, err)
	require.False(t, errors.Is(err, io.EOF))
}
