package executor

import "testing"

func TestCodexResolveUploadedImageMediaType(t *testing.T) {
	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpegBytes := []byte("\xff\xd8\xff\xe0\x00\x10JFIF")

	cases := []struct {
		name     string
		declared string
		data     []byte
		want     string
	}{
		{name: "octet-stream sniffs to png", declared: "application/octet-stream", data: pngBytes, want: "image/png"},
		{name: "empty sniffs to jpeg", declared: "", data: jpegBytes, want: "image/jpeg"},
		{name: "declared image kept", declared: "image/webp", data: pngBytes, want: "image/webp"},
		{name: "declared image with params stripped", declared: "image/png; name=foo", data: pngBytes, want: "image/png"},
		{name: "non-image declared and unsniffable defaults to png", declared: "application/octet-stream", data: []byte("not an image at all"), want: "image/png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexResolveUploadedImageMediaType(tc.declared, tc.data); got != tc.want {
				t.Fatalf("codexResolveUploadedImageMediaType(%q) = %q, want %q", tc.declared, got, tc.want)
			}
		})
	}
}
