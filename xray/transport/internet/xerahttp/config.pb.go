package xerahttp

import (
	internet "github.com/xtls/xray-core/transport/internet"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
	unsafe "unsafe"
)

const (
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type RangeConfig struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	From          int32                  `protobuf:"varint,1,opt,name=from,proto3" json:"from,omitempty"`
	To            int32                  `protobuf:"varint,2,opt,name=to,proto3" json:"to,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *RangeConfig) Reset() {
	*x = RangeConfig{}
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *RangeConfig) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*RangeConfig) ProtoMessage() {}

func (x *RangeConfig) ProtoReflect() protoreflect.Message {
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}
func (*RangeConfig) Descriptor() ([]byte, []int) {
	return file_transport_internet_xerahttp_config_proto_rawDescGZIP(), []int{0}
}

func (x *RangeConfig) GetFrom() int32 {
	if x != nil {
		return x.From
	}
	return 0
}

func (x *RangeConfig) GetTo() int32 {
	if x != nil {
		return x.To
	}
	return 0
}

type XmuxConfig struct {
	state            protoimpl.MessageState `protogen:"open.v1"`
	MaxConcurrency   *RangeConfig           `protobuf:"bytes,1,opt,name=maxConcurrency,proto3" json:"maxConcurrency,omitempty"`
	MaxConnections   *RangeConfig           `protobuf:"bytes,2,opt,name=maxConnections,proto3" json:"maxConnections,omitempty"`
	CMaxReuseTimes   *RangeConfig           `protobuf:"bytes,3,opt,name=cMaxReuseTimes,proto3" json:"cMaxReuseTimes,omitempty"`
	HMaxRequestTimes *RangeConfig           `protobuf:"bytes,4,opt,name=hMaxRequestTimes,proto3" json:"hMaxRequestTimes,omitempty"`
	HMaxReusableSecs *RangeConfig           `protobuf:"bytes,5,opt,name=hMaxReusableSecs,proto3" json:"hMaxReusableSecs,omitempty"`
	HKeepAlivePeriod int64                  `protobuf:"varint,6,opt,name=hKeepAlivePeriod,proto3" json:"hKeepAlivePeriod,omitempty"`
	unknownFields    protoimpl.UnknownFields
	sizeCache        protoimpl.SizeCache
}

func (x *XmuxConfig) Reset() {
	*x = XmuxConfig{}
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *XmuxConfig) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*XmuxConfig) ProtoMessage() {}

func (x *XmuxConfig) ProtoReflect() protoreflect.Message {
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}
func (*XmuxConfig) Descriptor() ([]byte, []int) {
	return file_transport_internet_xerahttp_config_proto_rawDescGZIP(), []int{1}
}

func (x *XmuxConfig) GetMaxConcurrency() *RangeConfig {
	if x != nil {
		return x.MaxConcurrency
	}
	return nil
}

func (x *XmuxConfig) GetMaxConnections() *RangeConfig {
	if x != nil {
		return x.MaxConnections
	}
	return nil
}

func (x *XmuxConfig) GetCMaxReuseTimes() *RangeConfig {
	if x != nil {
		return x.CMaxReuseTimes
	}
	return nil
}

func (x *XmuxConfig) GetHMaxRequestTimes() *RangeConfig {
	if x != nil {
		return x.HMaxRequestTimes
	}
	return nil
}

func (x *XmuxConfig) GetHMaxReusableSecs() *RangeConfig {
	if x != nil {
		return x.HMaxReusableSecs
	}
	return nil
}

func (x *XmuxConfig) GetHKeepAlivePeriod() int64 {
	if x != nil {
		return x.HKeepAlivePeriod
	}
	return 0
}

type Config struct {
	state                  protoimpl.MessageState `protogen:"open.v1"`
	Host                   string                 `protobuf:"bytes,1,opt,name=host,proto3" json:"host,omitempty"`
	Path                   string                 `protobuf:"bytes,2,opt,name=path,proto3" json:"path,omitempty"`
	Mode                   string                 `protobuf:"bytes,3,opt,name=mode,proto3" json:"mode,omitempty"`
	Headers                map[string]string      `protobuf:"bytes,4,rep,name=headers,proto3" json:"headers,omitempty" protobuf_key:"bytes,1,opt,name=key" protobuf_val:"bytes,2,opt,name=value"`
	XPaddingBytes          *RangeConfig           `protobuf:"bytes,5,opt,name=xPaddingBytes,proto3" json:"xPaddingBytes,omitempty"`
	NoGRPCHeader           bool                   `protobuf:"varint,6,opt,name=noGRPCHeader,proto3" json:"noGRPCHeader,omitempty"`
	NoSSEHeader            bool                   `protobuf:"varint,7,opt,name=noSSEHeader,proto3" json:"noSSEHeader,omitempty"`
	ScMaxEachPostBytes     *RangeConfig           `protobuf:"bytes,8,opt,name=scMaxEachPostBytes,proto3" json:"scMaxEachPostBytes,omitempty"`
	ScMinPostsIntervalMs   *RangeConfig           `protobuf:"bytes,9,opt,name=scMinPostsIntervalMs,proto3" json:"scMinPostsIntervalMs,omitempty"`
	ScMaxBufferedPosts     int64                  `protobuf:"varint,10,opt,name=scMaxBufferedPosts,proto3" json:"scMaxBufferedPosts,omitempty"`
	ScStreamUpServerSecs   *RangeConfig           `protobuf:"bytes,11,opt,name=scStreamUpServerSecs,proto3" json:"scStreamUpServerSecs,omitempty"`
	Xmux                   *XmuxConfig            `protobuf:"bytes,12,opt,name=xmux,proto3" json:"xmux,omitempty"`
	DownloadSettings       *internet.StreamConfig `protobuf:"bytes,13,opt,name=downloadSettings,proto3" json:"downloadSettings,omitempty"`
	XPaddingObfsMode       bool                   `protobuf:"varint,14,opt,name=xPaddingObfsMode,proto3" json:"xPaddingObfsMode,omitempty"`
	XPaddingKey            string                 `protobuf:"bytes,15,opt,name=xPaddingKey,proto3" json:"xPaddingKey,omitempty"`
	XPaddingHeader         string                 `protobuf:"bytes,16,opt,name=xPaddingHeader,proto3" json:"xPaddingHeader,omitempty"`
	XPaddingPlacement      string                 `protobuf:"bytes,17,opt,name=xPaddingPlacement,proto3" json:"xPaddingPlacement,omitempty"`
	XPaddingMethod         string                 `protobuf:"bytes,18,opt,name=xPaddingMethod,proto3" json:"xPaddingMethod,omitempty"`
	UplinkHTTPMethod       string                 `protobuf:"bytes,19,opt,name=uplinkHTTPMethod,proto3" json:"uplinkHTTPMethod,omitempty"`
	SessionIDPlacement     string                 `protobuf:"bytes,20,opt,name=sessionIDPlacement,proto3" json:"sessionIDPlacement,omitempty"`
	SessionIDKey           string                 `protobuf:"bytes,21,opt,name=sessionIDKey,proto3" json:"sessionIDKey,omitempty"`
	SeqPlacement           string                 `protobuf:"bytes,22,opt,name=seqPlacement,proto3" json:"seqPlacement,omitempty"`
	SeqKey                 string                 `protobuf:"bytes,23,opt,name=seqKey,proto3" json:"seqKey,omitempty"`
	UplinkDataPlacement    string                 `protobuf:"bytes,24,opt,name=uplinkDataPlacement,proto3" json:"uplinkDataPlacement,omitempty"`
	UplinkDataKey          string                 `protobuf:"bytes,25,opt,name=uplinkDataKey,proto3" json:"uplinkDataKey,omitempty"`
	UplinkChunkSize        *RangeConfig           `protobuf:"bytes,26,opt,name=uplinkChunkSize,proto3" json:"uplinkChunkSize,omitempty"`
	ServerMaxHeaderBytes   int32                  `protobuf:"varint,27,opt,name=serverMaxHeaderBytes,proto3" json:"serverMaxHeaderBytes,omitempty"`
	SessionIDTable         string                 `protobuf:"bytes,28,opt,name=sessionIDTable,proto3" json:"sessionIDTable,omitempty"`
	SessionIDLength        *RangeConfig           `protobuf:"bytes,29,opt,name=sessionIDLength,proto3" json:"sessionIDLength,omitempty"`
	CustomDownlinkPadding  *DownlinkPaddingConfig `protobuf:"bytes,30,opt,name=customDownlinkPadding,proto3" json:"customDownlinkPadding,omitempty"`
	ServerMaxBufferedBytes int64                  `protobuf:"varint,31,opt,name=serverMaxBufferedBytes,proto3" json:"serverMaxBufferedBytes,omitempty"`
	ServerMaxSessions      uint32                 `protobuf:"varint,32,opt,name=serverMaxSessions,proto3" json:"serverMaxSessions,omitempty"`
	unknownFields          protoimpl.UnknownFields
	sizeCache              protoimpl.SizeCache
}

func (x *Config) Reset() {
	*x = Config{}
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Config) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Config) ProtoMessage() {}

func (x *Config) ProtoReflect() protoreflect.Message {
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}
func (*Config) Descriptor() ([]byte, []int) {
	return file_transport_internet_xerahttp_config_proto_rawDescGZIP(), []int{2}
}

func (x *Config) GetHost() string {
	if x != nil {
		return x.Host
	}
	return ""
}

func (x *Config) GetPath() string {
	if x != nil {
		return x.Path
	}
	return ""
}

func (x *Config) GetMode() string {
	if x != nil {
		return x.Mode
	}
	return ""
}

func (x *Config) GetHeaders() map[string]string {
	if x != nil {
		return x.Headers
	}
	return nil
}

func (x *Config) GetXPaddingBytes() *RangeConfig {
	if x != nil {
		return x.XPaddingBytes
	}
	return nil
}

func (x *Config) GetNoGRPCHeader() bool {
	if x != nil {
		return x.NoGRPCHeader
	}
	return false
}

func (x *Config) GetNoSSEHeader() bool {
	if x != nil {
		return x.NoSSEHeader
	}
	return false
}

func (x *Config) GetScMaxEachPostBytes() *RangeConfig {
	if x != nil {
		return x.ScMaxEachPostBytes
	}
	return nil
}

func (x *Config) GetScMinPostsIntervalMs() *RangeConfig {
	if x != nil {
		return x.ScMinPostsIntervalMs
	}
	return nil
}

func (x *Config) GetScMaxBufferedPosts() int64 {
	if x != nil {
		return x.ScMaxBufferedPosts
	}
	return 0
}

func (x *Config) GetScStreamUpServerSecs() *RangeConfig {
	if x != nil {
		return x.ScStreamUpServerSecs
	}
	return nil
}

func (x *Config) GetXmux() *XmuxConfig {
	if x != nil {
		return x.Xmux
	}
	return nil
}

func (x *Config) GetDownloadSettings() *internet.StreamConfig {
	if x != nil {
		return x.DownloadSettings
	}
	return nil
}

func (x *Config) GetXPaddingObfsMode() bool {
	if x != nil {
		return x.XPaddingObfsMode
	}
	return false
}

func (x *Config) GetXPaddingKey() string {
	if x != nil {
		return x.XPaddingKey
	}
	return ""
}

func (x *Config) GetXPaddingHeader() string {
	if x != nil {
		return x.XPaddingHeader
	}
	return ""
}

func (x *Config) GetXPaddingPlacement() string {
	if x != nil {
		return x.XPaddingPlacement
	}
	return ""
}

func (x *Config) GetXPaddingMethod() string {
	if x != nil {
		return x.XPaddingMethod
	}
	return ""
}

func (x *Config) GetUplinkHTTPMethod() string {
	if x != nil {
		return x.UplinkHTTPMethod
	}
	return ""
}

func (x *Config) GetSessionIDPlacement() string {
	if x != nil {
		return x.SessionIDPlacement
	}
	return ""
}

func (x *Config) GetSessionIDKey() string {
	if x != nil {
		return x.SessionIDKey
	}
	return ""
}

func (x *Config) GetSeqPlacement() string {
	if x != nil {
		return x.SeqPlacement
	}
	return ""
}

func (x *Config) GetSeqKey() string {
	if x != nil {
		return x.SeqKey
	}
	return ""
}

func (x *Config) GetUplinkDataPlacement() string {
	if x != nil {
		return x.UplinkDataPlacement
	}
	return ""
}

func (x *Config) GetUplinkDataKey() string {
	if x != nil {
		return x.UplinkDataKey
	}
	return ""
}

func (x *Config) GetUplinkChunkSize() *RangeConfig {
	if x != nil {
		return x.UplinkChunkSize
	}
	return nil
}

func (x *Config) GetServerMaxHeaderBytes() int32 {
	if x != nil {
		return x.ServerMaxHeaderBytes
	}
	return 0
}

func (x *Config) GetSessionIDTable() string {
	if x != nil {
		return x.SessionIDTable
	}
	return ""
}

func (x *Config) GetSessionIDLength() *RangeConfig {
	if x != nil {
		return x.SessionIDLength
	}
	return nil
}

func (x *Config) GetCustomDownlinkPadding() *DownlinkPaddingConfig {
	if x != nil {
		return x.CustomDownlinkPadding
	}
	return nil
}

func (x *Config) GetServerMaxBufferedBytes() int64 {
	if x != nil {
		return x.ServerMaxBufferedBytes
	}
	return 0
}

func (x *Config) GetServerMaxSessions() uint32 {
	if x != nil {
		return x.ServerMaxSessions
	}
	return 0
}

type DownlinkPaddingConfig struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Header        string                 `protobuf:"bytes,1,opt,name=header,proto3" json:"header,omitempty"`
	Token         string                 `protobuf:"bytes,2,opt,name=token,proto3" json:"token,omitempty"`
	Bytes         *RangeConfig           `protobuf:"bytes,3,opt,name=bytes,proto3" json:"bytes,omitempty"`
	BlockBytes    *RangeConfig           `protobuf:"bytes,4,opt,name=blockBytes,proto3" json:"blockBytes,omitempty"`
	BudgetPercent uint32                 `protobuf:"varint,5,opt,name=budgetPercent,proto3" json:"budgetPercent,omitempty"`
	BurstBytes    uint32                 `protobuf:"varint,6,opt,name=burstBytes,proto3" json:"burstBytes,omitempty"`
	Uplink        bool                   `protobuf:"varint,7,opt,name=uplink,proto3" json:"uplink,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *DownlinkPaddingConfig) Reset() {
	*x = DownlinkPaddingConfig{}
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[3]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *DownlinkPaddingConfig) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*DownlinkPaddingConfig) ProtoMessage() {}

func (x *DownlinkPaddingConfig) ProtoReflect() protoreflect.Message {
	mi := &file_transport_internet_xerahttp_config_proto_msgTypes[3]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}
func (*DownlinkPaddingConfig) Descriptor() ([]byte, []int) {
	return file_transport_internet_xerahttp_config_proto_rawDescGZIP(), []int{3}
}

func (x *DownlinkPaddingConfig) GetHeader() string {
	if x != nil {
		return x.Header
	}
	return ""
}

func (x *DownlinkPaddingConfig) GetToken() string {
	if x != nil {
		return x.Token
	}
	return ""
}

func (x *DownlinkPaddingConfig) GetBytes() *RangeConfig {
	if x != nil {
		return x.Bytes
	}
	return nil
}

func (x *DownlinkPaddingConfig) GetBlockBytes() *RangeConfig {
	if x != nil {
		return x.BlockBytes
	}
	return nil
}

func (x *DownlinkPaddingConfig) GetBudgetPercent() uint32 {
	if x != nil {
		return x.BudgetPercent
	}
	return 0
}

func (x *DownlinkPaddingConfig) GetBurstBytes() uint32 {
	if x != nil {
		return x.BurstBytes
	}
	return 0
}

func (x *DownlinkPaddingConfig) GetUplink() bool {
	if x != nil {
		return x.Uplink
	}
	return false
}

var File_transport_internet_xerahttp_config_proto protoreflect.FileDescriptor

const file_transport_internet_xerahttp_config_proto_rawDesc = "" +
	"\n" +
	"(transport/internet/xerahttp/config.proto\x12 xray.transport.internet.xerahttp\x1a\x1ftransport/internet/config.proto\"1\n" +
	"\vRangeConfig\x12\x12\n" +
	"\x04from\x18\x01 \x01(\x05R\x04from\x12\x0e\n" +
	"\x02to\x18\x02 \x01(\x05R\x02to\"\xf3\x03\n" +
	"\n" +
	"XmuxConfig\x12U\n" +
	"\x0emaxConcurrency\x18\x01 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x0emaxConcurrency\x12U\n" +
	"\x0emaxConnections\x18\x02 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x0emaxConnections\x12U\n" +
	"\x0ecMaxReuseTimes\x18\x03 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x0ecMaxReuseTimes\x12Y\n" +
	"\x10hMaxRequestTimes\x18\x04 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x10hMaxRequestTimes\x12Y\n" +
	"\x10hMaxReusableSecs\x18\x05 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x10hMaxReusableSecs\x12*\n" +
	"\x10hKeepAlivePeriod\x18\x06 \x01(\x03R\x10hKeepAlivePeriod\"\x99\x0e\n" +
	"\x06Config\x12\x12\n" +
	"\x04host\x18\x01 \x01(\tR\x04host\x12\x12\n" +
	"\x04path\x18\x02 \x01(\tR\x04path\x12\x12\n" +
	"\x04mode\x18\x03 \x01(\tR\x04mode\x12O\n" +
	"\aheaders\x18\x04 \x03(\v25.xray.transport.internet.xerahttp.Config.HeadersEntryR\aheaders\x12S\n" +
	"\rxPaddingBytes\x18\x05 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\rxPaddingBytes\x12\"\n" +
	"\fnoGRPCHeader\x18\x06 \x01(\bR\fnoGRPCHeader\x12 \n" +
	"\vnoSSEHeader\x18\a \x01(\bR\vnoSSEHeader\x12]\n" +
	"\x12scMaxEachPostBytes\x18\b \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x12scMaxEachPostBytes\x12a\n" +
	"\x14scMinPostsIntervalMs\x18\t \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x14scMinPostsIntervalMs\x12.\n" +
	"\x12scMaxBufferedPosts\x18\n" +
	" \x01(\x03R\x12scMaxBufferedPosts\x12a\n" +
	"\x14scStreamUpServerSecs\x18\v \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x14scStreamUpServerSecs\x12@\n" +
	"\x04xmux\x18\f \x01(\v2,.xray.transport.internet.xerahttp.XmuxConfigR\x04xmux\x12Q\n" +
	"\x10downloadSettings\x18\r \x01(\v2%.xray.transport.internet.StreamConfigR\x10downloadSettings\x12*\n" +
	"\x10xPaddingObfsMode\x18\x0e \x01(\bR\x10xPaddingObfsMode\x12 \n" +
	"\vxPaddingKey\x18\x0f \x01(\tR\vxPaddingKey\x12&\n" +
	"\x0exPaddingHeader\x18\x10 \x01(\tR\x0exPaddingHeader\x12,\n" +
	"\x11xPaddingPlacement\x18\x11 \x01(\tR\x11xPaddingPlacement\x12&\n" +
	"\x0exPaddingMethod\x18\x12 \x01(\tR\x0exPaddingMethod\x12*\n" +
	"\x10uplinkHTTPMethod\x18\x13 \x01(\tR\x10uplinkHTTPMethod\x12.\n" +
	"\x12sessionIDPlacement\x18\x14 \x01(\tR\x12sessionIDPlacement\x12\"\n" +
	"\fsessionIDKey\x18\x15 \x01(\tR\fsessionIDKey\x12\"\n" +
	"\fseqPlacement\x18\x16 \x01(\tR\fseqPlacement\x12\x16\n" +
	"\x06seqKey\x18\x17 \x01(\tR\x06seqKey\x120\n" +
	"\x13uplinkDataPlacement\x18\x18 \x01(\tR\x13uplinkDataPlacement\x12$\n" +
	"\ruplinkDataKey\x18\x19 \x01(\tR\ruplinkDataKey\x12W\n" +
	"\x0fuplinkChunkSize\x18\x1a \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x0fuplinkChunkSize\x122\n" +
	"\x14serverMaxHeaderBytes\x18\x1b \x01(\x05R\x14serverMaxHeaderBytes\x12&\n" +
	"\x0esessionIDTable\x18\x1c \x01(\tR\x0esessionIDTable\x12W\n" +
	"\x0fsessionIDLength\x18\x1d \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x0fsessionIDLength\x12m\n" +
	"\x15customDownlinkPadding\x18\x1e \x01(\v27.xray.transport.internet.xerahttp.DownlinkPaddingConfigR\x15customDownlinkPadding\x126\n" +
	"\x16serverMaxBufferedBytes\x18\x1f \x01(\x03R\x16serverMaxBufferedBytes\x12,\n" +
	"\x11serverMaxSessions\x18  \x01(\rR\x11serverMaxSessions\x1a:\n" +
	"\fHeadersEntry\x12\x10\n" +
	"\x03key\x18\x01 \x01(\tR\x03key\x12\x14\n" +
	"\x05value\x18\x02 \x01(\tR\x05value:\x028\x01\"\xb7\x02\n" +
	"\x15DownlinkPaddingConfig\x12\x16\n" +
	"\x06header\x18\x01 \x01(\tR\x06header\x12\x14\n" +
	"\x05token\x18\x02 \x01(\tR\x05token\x12C\n" +
	"\x05bytes\x18\x03 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\x05bytes\x12M\n" +
	"\n" +
	"blockBytes\x18\x04 \x01(\v2-.xray.transport.internet.xerahttp.RangeConfigR\n" +
	"blockBytes\x12$\n" +
	"\rbudgetPercent\x18\x05 \x01(\rR\rbudgetPercent\x12\x1e\n" +
	"\n" +
	"burstBytes\x18\x06 \x01(\rR\n" +
	"burstBytes\x12\x16\n" +
	"\x06uplink\x18\a \x01(\bR\x06uplinkB\x82\x01\n" +
	"$com.xray.transport.internet.xerahttpP\x01Z5github.com/xtls/xray-core/transport/internet/xerahttp\xaa\x02 Xray.Transport.Internet.XeraHttpb\x06proto3"

var (
	file_transport_internet_xerahttp_config_proto_rawDescOnce sync.Once
	file_transport_internet_xerahttp_config_proto_rawDescData []byte
)

func file_transport_internet_xerahttp_config_proto_rawDescGZIP() []byte {
	file_transport_internet_xerahttp_config_proto_rawDescOnce.Do(func() {
		file_transport_internet_xerahttp_config_proto_rawDescData = protoimpl.X.CompressGZIP(unsafe.Slice(unsafe.StringData(file_transport_internet_xerahttp_config_proto_rawDesc), len(file_transport_internet_xerahttp_config_proto_rawDesc)))
	})
	return file_transport_internet_xerahttp_config_proto_rawDescData
}

var file_transport_internet_xerahttp_config_proto_msgTypes = make([]protoimpl.MessageInfo, 5)
var file_transport_internet_xerahttp_config_proto_goTypes = []any{
	(*RangeConfig)(nil),
	(*XmuxConfig)(nil),
	(*Config)(nil),
	(*DownlinkPaddingConfig)(nil),
	nil,
	(*internet.StreamConfig)(nil),
}
var file_transport_internet_xerahttp_config_proto_depIdxs = []int32{
	0,
	0,
	0,
	0,
	0,
	4,
	0,
	0,
	0,
	0,
	1,
	5,
	0,
	0,
	3,
	0,
	0,
	17,
	17,
	17,
	17,
	0,
}

func init() { file_transport_internet_xerahttp_config_proto_init() }
func file_transport_internet_xerahttp_config_proto_init() {
	if File_transport_internet_xerahttp_config_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_transport_internet_xerahttp_config_proto_rawDesc), len(file_transport_internet_xerahttp_config_proto_rawDesc)),
			NumEnums:      0,
			NumMessages:   5,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_transport_internet_xerahttp_config_proto_goTypes,
		DependencyIndexes: file_transport_internet_xerahttp_config_proto_depIdxs,
		MessageInfos:      file_transport_internet_xerahttp_config_proto_msgTypes,
	}.Build()
	File_transport_internet_xerahttp_config_proto = out.File
	file_transport_internet_xerahttp_config_proto_goTypes = nil
	file_transport_internet_xerahttp_config_proto_depIdxs = nil
}
