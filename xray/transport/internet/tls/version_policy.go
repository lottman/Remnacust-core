package tls

import (
	stdtls "crypto/tls"
	"errors"
	"net"
	"slices"

	utls "github.com/refraction-networking/utls"
)

type tlsPolicyFailureConn struct {
	net.Conn
	err error
}

func (c *tlsPolicyFailureConn) Read([]byte) (int, error)  { return 0, c.err }
func (c *tlsPolicyFailureConn) Write([]byte) (int, error) { return 0, c.err }

func newUTLSClient(raw net.Conn, config *stdtls.Config, fingerprint utls.ClientHelloID) *utls.UConn {
	utlsConfig := copyConfig(config)
	client := utls.UClient(raw, utlsConfig, fingerprint)
	if (config.MinVersion != 0 || config.MaxVersion != 0) && config.EncryptedClientHelloConfigList == nil {
		requestedALPN := slices.Clone(utlsConfig.NextProtos)
		err := constrainUTLSVersions(client, config.MinVersion, config.MaxVersion)
		utlsConfig.NextProtos = requestedALPN
		if err != nil {
			client.SetUnderlyingConn(&tlsPolicyFailureConn{Conn: raw, err: err})
		}
	}
	return client
}

func constrainUTLSVersions(client *utls.UConn, minimum, maximum uint16) error {
	if err := client.BuildHandshakeState(); err != nil {
		return err
	}
	versions := client.HandshakeState.Hello.SupportedVersions
	var extension *utls.SupportedVersionsExtension
	for _, candidate := range client.Extensions {
		if value, ok := candidate.(*utls.SupportedVersionsExtension); ok {
			extension = value
			versions = value.Versions
			break
		}
	}
	allowed := make([]uint16, 0, len(versions))
	var first, last uint16
	for _, version := range versions {
		if version&0x0f0f == 0x0a0a && byte(version>>8) == byte(version) {
			allowed = append(allowed, version)
			continue
		}
		if minimum != 0 && version < minimum || maximum != 0 && version > maximum {
			continue
		}
		allowed = append(allowed, version)
		if first == 0 || version < first {
			first = version
		}
		if version > last {
			last = version
		}
	}
	if first == 0 {
		return errors.New("TLS fingerprint cannot satisfy the configured version policy")
	}
	if extension != nil {
		extension.Versions = allowed
	}
	if err := client.SetTLSVers(first, last, client.Extensions); err != nil {
		return err
	}
	client.HandshakeState.Hello.Vers = min(last, stdtls.VersionTLS12)
	return client.BuildHandshakeState()
}
