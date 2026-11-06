package xerahttp

import (
	"fmt"
	"strings"

	"golang.org/x/net/http/httpguts"
)

func metadataHeaderValid(name string) bool {
	if len(name) < 3 || len(name) > 64 || !strings.HasPrefix(strings.ToLower(name), "x-") || !httpguts.ValidHeaderFieldName(name) {
		return false
	}
	switch strings.ToLower(name) {
	case "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "x-accel-buffering":
		return false
	}
	return true
}

func (c *Config) validateMetadata() error {
	if !metadataHeaderValid(c.GetNormalizedXPaddingHeader()) {
		return fmt.Errorf("xPaddingHeader must be a non-proxy X- header of 3..64 characters")
	}
	seen := make(map[string]bool)
	for index, item := range []struct{ placement, key string }{
		{c.GetNormalizedSessionPlacement(), c.GetNormalizedSessionKey()},
		{c.GetNormalizedSeqPlacement(), c.GetNormalizedSeqKey()},
		{c.GetNormalizedXPaddingPlacement(), c.GetNormalizedXPaddingKey()},
	} {
		switch item.placement {
		case PlacementPath:
			if index == 2 {
				return fmt.Errorf("padding cannot use path placement")
			}
			continue
		case PlacementHeader, PlacementQueryInHeader:
			name := item.key
			if index == 2 {
				name = c.GetNormalizedXPaddingHeader()
			} else if item.placement == PlacementQueryInHeader {
				return fmt.Errorf("queryInHeader is supported only for padding")
			}
			if !metadataHeaderValid(name) {
				return fmt.Errorf("metadata header must be a non-proxy X- header of 3..64 characters")
			}
			identity := PlacementHeader + ":" + strings.ToLower(name)
			if seen[identity] {
				return fmt.Errorf("metadata headers must have distinct names")
			}
			seen[identity] = true
		case PlacementQuery, PlacementCookie:
			if len(item.key) == 0 || len(item.key) > 64 || !httpguts.ValidHeaderFieldName(item.key) {
				return fmt.Errorf("metadata keys must be HTTP tokens of 1..64 characters")
			}
			identity := item.placement + ":" + item.key
			if seen[identity] {
				return fmt.Errorf("metadata keys in the same placement must be distinct")
			}
			seen[identity] = true
		default:
			return fmt.Errorf("unsupported metadata placement: %s", item.placement)
		}
	}
	if padding := c.CustomDownlinkPadding; padding != nil {
		if strings.EqualFold(padding.Header, c.GetNormalizedXPaddingHeader()) || seen[PlacementHeader+":"+strings.ToLower(padding.Header)] {
			return fmt.Errorf("customDownlinkPadding.header conflicts with transport metadata")
		}
	}
	return nil
}
