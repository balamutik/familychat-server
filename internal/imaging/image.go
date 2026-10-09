package imaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxPNGBytes = 5 << 20

var ErrInvalidImage = errors.New("invalid image")

func MediaType(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
}

func Supported(value string) bool {
	switch MediaType(value) {
	case "image/jpeg", "image/png", "image/apng", "image/gif", "image/webp",
		"image/heic", "image/heif", "image/heic-sequence", "image/heif-sequence",
		"image/avif", "image/avif-sequence", "image/bmp", "image/x-ms-bmp",
		"image/tiff", "image/x-icon", "image/vnd.microsoft.icon", "image/ico", "image/svg+xml":
		return true
	default:
		return false
	}
}

// ConvertBytesToPNG is used for avatars. The returned bytes are a decoded,
// bounded PNG, so a public avatar never serves active SVG or an unsupported codec.
func ConvertBytesToPNG(ctx context.Context, data []byte, mediaType string, maxEdge int) ([]byte, error) {
	dir, err := os.MkdirTemp("", "familychat-avatar-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "input")
	out := filepath.Join(dir, "output.png")
	if err := os.WriteFile(in, data, 0600); err != nil {
		return nil, err
	}
	if err := ConvertFileToPNG(ctx, in, out, mediaType, maxEdge, "ffmpeg"); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// ConvertFileToPNG creates a first-frame preview without using the file name
// or client-supplied MIME alone to select a decoder.
func ConvertFileToPNG(ctx context.Context, inputPath, outputPath, mediaType string, maxEdge int, ffmpeg string) error {
	mediaType = MediaType(mediaType)
	if !Supported(mediaType) || maxEdge < 1 || maxEdge > 1024 {
		return ErrInvalidImage
	}
	valid, err := matchesFile(inputPath, mediaType)
	if err != nil || !valid {
		return ErrInvalidImage
	}
	dir, err := os.MkdirTemp("", "familychat-convert-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	source := inputPath
	switch mediaType {
	case "image/heic", "image/heif", "image/heic-sequence", "image/heif-sequence":
		source = filepath.Join(dir, "decoded.png")
		if err := exec.CommandContext(ctx, "heif-convert", inputPath, source).Run(); err != nil {
			return fmt.Errorf("HEIF decode: %w", err)
		}
		if info, err := os.Stat(source); err != nil || info.Size() > 100<<20 {
			return ErrInvalidImage
		}
	case "image/svg+xml":
		// Librsvg may read files next to the SVG. Give it an empty private directory.
		isolated := filepath.Join(dir, "input.svg")
		in, err := os.Open(inputPath)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(isolated, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(in, 16<<20+1))
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			return ErrInvalidImage
		}
		if info, err := os.Stat(isolated); err != nil || info.Size() > 16<<20 {
			return ErrInvalidImage
		}
		source = filepath.Join(dir, "decoded.png")
		cmd := exec.CommandContext(ctx, "rsvg-convert", "-a", "-w", fmt.Sprint(maxEdge), "-h", fmt.Sprint(maxEdge), "-o", source, isolated)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("SVG decode: %w", err)
		}
	}
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	filter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", maxEdge, maxEdge)
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-y", "-i", source,
		"-frames:v", "1", "-vf", filter, "-c:v", "png", "-update", "1", outputPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("image decode: %w", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil || info.Size() < 1 || info.Size() > maxPNGBytes {
		return ErrInvalidImage
	}
	file, err := os.Open(outputPath)
	if err != nil {
		return err
	}
	defer file.Close()
	config, err := png.DecodeConfig(file)
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxEdge || config.Height > maxEdge {
		return ErrInvalidImage
	}
	return nil
}

func matchesFile(path, mediaType string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return false, err
	}
	switch mediaType {
	case "image/jpeg":
		return len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}), nil
	case "image/png", "image/apng":
		return len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")), nil
	case "image/gif":
		return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")), nil
	case "image/webp":
		return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")), nil
	case "image/bmp", "image/x-ms-bmp":
		return bytes.HasPrefix(data, []byte("BM")), nil
	case "image/tiff":
		return bytes.HasPrefix(data, []byte("II*\x00")) || bytes.HasPrefix(data, []byte("MM\x00*")), nil
	case "image/x-icon", "image/vnd.microsoft.icon", "image/ico":
		return bytes.HasPrefix(data, []byte{0, 0, 1, 0}), nil
	case "image/heic", "image/heif", "image/heic-sequence", "image/heif-sequence",
		"image/avif", "image/avif-sequence":
		return matchesHEIFBrand(data, mediaType), nil
	case "image/svg+xml":
		decoder := xml.NewDecoder(bytes.NewReader(data))
		for {
			token, err := decoder.Token()
			if err != nil {
				return false, nil
			}
			if root, ok := token.(xml.StartElement); ok {
				return root.Name.Local == "svg" && (root.Name.Space == "" || root.Name.Space == "http://www.w3.org/2000/svg"), nil
			}
		}
	default:
		return false, nil
	}
}

func matchesHEIFBrand(data []byte, mediaType string) bool {
	if len(data) < 16 || !bytes.Equal(data[4:8], []byte("ftyp")) {
		return false
	}
	boxSize := int(binary.BigEndian.Uint32(data[:4]))
	if boxSize < 16 || boxSize > len(data) {
		return false
	}
	for i := 8; i+4 <= boxSize; i += 4 {
		brand := string(data[i : i+4])
		switch mediaType {
		case "image/avif", "image/avif-sequence":
			if brand == "avif" || brand == "avis" {
				return true
			}
		case "image/heif", "image/heif-sequence":
			if brand == "heic" || brand == "heix" || brand == "hevc" || brand == "hevx" || brand == "mif1" || brand == "msf1" {
				return true
			}
		case "image/heic", "image/heic-sequence":
			if brand == "heic" || brand == "heix" || brand == "hevc" || brand == "hevx" {
				return true
			}
		}
	}
	return false
}
