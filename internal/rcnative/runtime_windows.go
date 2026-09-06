//go:build windows

package rcnative

import (
	"encoding/base64"
	"encoding/binary"
	"unicode/utf16"
)

func current() Runtime {
	return Runtime{
		Home:      `C:\home\agent`,
		Workspace: `C:\workspace`,
		Run:       `C:\run\rc`,
		Endpoint:  `\\.\pipe\rc-kube`,
	}
}

func scriptCommand(script string) []string {
	words := utf16.Encode([]rune("$ErrorActionPreference = 'Stop'; & {\n" + script + "\n}; if ($LASTEXITCODE) { exit $LASTEXITCODE }"))
	data := make([]byte, len(words)*2)
	for i, word := range words {
		binary.LittleEndian.PutUint16(data[i*2:], word)
	}
	return []string{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(data)}
}
