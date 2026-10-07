package conf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"github.com/xtls/xray-core/transport/internet/finalmask/udphop"
)

func TestSS2022ExplicitEmptyClientsKeepsDynamicUserManagement(t *testing.T) {
	for _, method := range []string{"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm"} {
		var config ShadowsocksServerConfig
		require.NoError(t, json.Unmarshal([]byte(`{"method":"`+method+`","password":"server-key","clients":[]}`), &config))
		built, err := config.Build()
		require.NoError(t, err)
		managed, ok := built.(*shadowsocks_2022.MultiUserServerConfig)
		require.True(t, ok)
		require.Empty(t, managed.Users)
		require.Equal(t, method, managed.Method)
	}
	var single ShadowsocksServerConfig
	require.NoError(t, json.Unmarshal([]byte(`{"method":"2022-blake3-aes-256-gcm","password":"server-key"}`), &single))
	built, err := single.Build()
	require.NoError(t, err)
	_, ok := built.(*shadowsocks_2022.ServerConfig)
	require.True(t, ok)
}

func TestLegacyUDPHopMigratesWithoutLosingMaskOrDuplicatingIt(t *testing.T) {
	var config StreamConfig
	require.NoError(t, json.Unmarshal([]byte(`{"network":"xera-http","xeraHttpSettings":{"path":"/"},"finalmask":{"quicParams":{"udpHop":{"ports":"40000-40002","interval":"30-60"}}}}`), &config))
	for range 2 {
		built, err := config.Build()
		require.NoError(t, err)
		require.Len(t, built.Udpmasks, 1)
		message, err := built.Udpmasks[0].GetInstance()
		require.NoError(t, err)
		hop := message.(*udphop.Config)
		require.Equal(t, []uint32{40000, 40001, 40002}, hop.RemotePorts)
		require.EqualValues(t, 30, hop.IntervalMin)
		require.EqualValues(t, 60, hop.IntervalMax)
		require.True(t, hop.Remote && hop.RemoteOnce)
	}
}

func TestConflictingLegacyUDPHopIsRejected(t *testing.T) {
	var config StreamConfig
	require.NoError(t, json.Unmarshal([]byte(`{"finalmask":{"udp":[{"type":"udpHop","settings":{"mode":"perConnRemote","remotePorts":"40000"}}],"quicParams":{"udpHop":{"ports":"40001"}}}}`), &config))
	_, err := config.Build()
	require.ErrorContains(t, err, "not both")
}
