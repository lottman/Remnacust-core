package xdrive

import (
	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/net/http/httpguts"
	"net"
	"net/url"
	"strings"
)

func ValidateS3Config(c *Config) error {
	if c == nil || c.S3 == nil {
		return errors.New(`service "s3" needs an "s3" object`)
	}
	u, err := url.Parse(c.S3.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("S3 endpoint must be an http(s) URL without credentials, query or fragment")
	}
	if !httpguts.ValidHeaderFieldValue(c.S3.Region) || strings.ContainsAny(c.S3.Region, "/, \t") {
		return errors.New("invalid S3 signing region")
	}
	bucket := c.S3.Bucket
	if !validS3Name(bucket) || strings.Contains(bucket, "/") {
		return errors.New("S3 needs a bucket name without path separators")
	}
	if !c.S3.PathStyle {
		if net.ParseIP(u.Hostname()) != nil {
			return errors.New("S3 virtual-hosted addressing requires a DNS endpoint")
		}
		// In this mode the bucket is part of the HTTP authority, not its path.
		if strings.Trim(bucket, "abcdefghijklmnopqrstuvwxyz0123456789-.") != "" || strings.Contains(bucket, "..") ||
			strings.HasPrefix(bucket, ".") || strings.HasSuffix(bucket, ".") {
			return errors.New("S3 virtual-hosted addressing requires a DNS-compatible bucket name")
		}
	}
	if !validS3Name(strings.TrimSuffix(c.RemoteFolder, "/")) {
		return errors.New("S3 remoteFolder must be a nonempty relative prefix without empty, '.' or '..' components")
	}
	if len(c.Secrets) < 2 || len(c.Secrets) > 3 || c.Secrets[0] == "" || c.Secrets[1] == "" {
		return errors.New("S3 needs AccessKeyID, SecretAccessKey and optionally SessionToken in secrets")
	}
	for _, secret := range c.Secrets {
		if !httpguts.ValidHeaderFieldValue(secret) {
			return errors.New("invalid S3 secret")
		}
	}
	if strings.ContainsAny(c.Secrets[0], "/ ,\t") {
		return errors.New("invalid S3 AccessKeyID")
	}
	return nil
}

func validS3Name(name string) bool {
	for _, ch := range name {
		if ch < 0x20 || ch == 0x7f || ch == '\\' {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
