package tls

import (
	"bytes"
	"compress/zlib"
	"io"
	"math/rand"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func TestRemnacustCertificateCompression(t *testing.T) {
	codecs := []struct {
		name      string
		algorithm CertCompressionAlgo
		writer    func(*testing.T, io.Writer) io.WriteCloser
	}{
		{"brotli", CertCompressionBrotli, func(_ *testing.T, w io.Writer) io.WriteCloser { return brotli.NewWriterLevel(w, 1) }},
		{"zlib", CertCompressionZlib, func(_ *testing.T, w io.Writer) io.WriteCloser { return zlib.NewWriter(w) }},
		{"zstd", CertCompressionZstd, func(t *testing.T, w io.Writer) io.WriteCloser {
			encoder, err := zstd.NewWriter(w, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
			if err != nil {
				t.Fatal(err)
			}
			return encoder
		}},
	}
	randomCertificate := make([]byte, 48<<10)
	rand.New(rand.NewSource(7)).Read(randomCertificate)
	for _, codec := range codecs {
		t.Run(codec.name, func(t *testing.T) {
			t.Parallel()
			encode := func(body []byte) []byte {
				var output bytes.Buffer
				writer := codec.writer(t, &output)
				if _, err := writer.Write(body); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				return output.Bytes()
			}
			for _, certificate := range []struct {
				name string
				data []byte
			}{
				{"small", []byte("certificate")},
				{"multiple_input_buffers", randomCertificate},
				{"large_compressible", bytes.Repeat([]byte("certificate"), 32<<10)},
			} {
				t.Run(certificate.name, func(t *testing.T) {
					raw, err := (&certificateMsgTLS13{certificate: Certificate{Certificate: [][]byte{certificate.data}}}).marshal()
					if err != nil {
						t.Fatal(err)
					}
					body := raw[4:]
					encoded := encode(body)
					if certificate.name == "multiple_input_buffers" && len(encoded) <= 32<<10 {
						t.Fatal("fixture does not cross the input buffer boundary")
					}
					messages := []struct {
						name   string
						data   []byte
						length uint32
						valid  bool
					}{
						{"valid", encoded, uint32(len(body)), true},
						{"truncated_stream", encoded[:len(encoded)-1], uint32(len(body)), false},
						{"short_stream", encoded[:len(encoded)/2], uint32(len(body)), false},
						{"advertised_too_large", encoded, uint32(len(body) + 1), false},
						{"advertised_too_small", encoded, uint32(len(body) - 1), false},
						{"extra_output", encode(append(bytes.Clone(body), 0)), uint32(len(body)), false},
						{"trailing_input", append(bytes.Clone(encoded), 0, 1), uint32(len(body)), false},
					}
					if codec.algorithm != CertCompressionBrotli {
						corrupt := bytes.Clone(encoded)
						corrupt[len(corrupt)-1] ^= 0xff
						messages = append(messages, struct {
							name   string
							data   []byte
							length uint32
							valid  bool
						}{"bad_checksum", corrupt, uint32(len(body)), false})
					}
					for _, message := range messages {
						t.Run(message.name, func(t *testing.T) {
							hs := &clientHandshakeStateTLS13{c: &Conn{buffering: true, config: &Config{}}, uconn: &UConn{certCompressionAlgs: []CertCompressionAlgo{codec.algorithm}}}
							got, err := hs.decompressCert(utlsCompressedCertificateMsg{algorithm: uint16(codec.algorithm), uncompressedLength: message.length, compressedCertificateMessage: message.data})
							if message.valid {
								if err != nil {
									t.Fatal(err)
								}
								if got == nil || len(got.certificate.Certificate) != 1 || !bytes.Equal(got.certificate.Certificate[0], certificate.data) {
									t.Fatal("certificate changed")
								}
								if len(hs.c.sendBuf) != 0 {
									t.Fatal("valid message produced a TLS alert")
								}
							} else {
								if err == nil || got != nil {
									t.Fatal("invalid compressed certificate accepted")
								}
								if len(hs.c.sendBuf) < 2 || hs.c.sendBuf[len(hs.c.sendBuf)-1] != byte(alertBadCertificate) {
									t.Fatal("missing bad_certificate alert")
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestRemnacustCertificateCompressionTrailingBoundary(t *testing.T) {
	certificate := bytes.Repeat([]byte("a"), 32755)
	raw, err := (&certificateMsgTLS13{certificate: Certificate{Certificate: [][]byte{certificate}}}).marshal()
	if err != nil {
		t.Fatal(err)
	}
	body := raw[4:]
	encoded := append([]byte{0x8b, 0xfd, 0xbf}, body...)
	encoded = append(encoded, 0x03)
	if len(encoded) != 32<<10 {
		t.Fatal("fixture does not end at the decoder input boundary")
	}
	for _, trailing := range []bool{false, true} {
		message := bytes.Clone(encoded)
		if trailing {
			message = append(message, 0, 1)
		}
		hs := &clientHandshakeStateTLS13{c: &Conn{buffering: true, config: &Config{}}, uconn: &UConn{certCompressionAlgs: []CertCompressionAlgo{CertCompressionBrotli}}}
		got, err := hs.decompressCert(utlsCompressedCertificateMsg{algorithm: uint16(CertCompressionBrotli), uncompressedLength: uint32(len(body)), compressedCertificateMessage: message})
		if trailing {
			if err == nil || got != nil {
				t.Fatal("accepted trailing compressed bytes at the input boundary")
			}
		} else if err != nil || got == nil || len(got.certificate.Certificate) != 1 || !bytes.Equal(got.certificate.Certificate[0], certificate) {
			t.Fatal("rejected valid independent boundary fixture", err)
		}
	}
}
