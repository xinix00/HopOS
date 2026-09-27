//go:build !embedcodec

package codecblob

// Geen ingebakken codec-blobs: de node leest zijn firmware van het volume.
func firmware(string) []byte { return nil }
func clip() []byte           { return nil }
