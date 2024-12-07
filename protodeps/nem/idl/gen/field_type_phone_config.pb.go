// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.35.2
// 	protoc        v5.28.3
// source: field_type_phone_config.proto

package gen

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type FieldTypePhoneConfig struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	AllowCountries   []string `protobuf:"bytes,1,rep,name=allow_countries,json=allowCountries,proto3" json:"allow_countries,omitempty"`
	ExcludeCountries []string `protobuf:"bytes,2,rep,name=exclude_countries,json=excludeCountries,proto3" json:"exclude_countries,omitempty"`
}

func (x *FieldTypePhoneConfig) Reset() {
	*x = FieldTypePhoneConfig{}
	mi := &file_field_type_phone_config_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *FieldTypePhoneConfig) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*FieldTypePhoneConfig) ProtoMessage() {}

func (x *FieldTypePhoneConfig) ProtoReflect() protoreflect.Message {
	mi := &file_field_type_phone_config_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use FieldTypePhoneConfig.ProtoReflect.Descriptor instead.
func (*FieldTypePhoneConfig) Descriptor() ([]byte, []int) {
	return file_field_type_phone_config_proto_rawDescGZIP(), []int{0}
}

func (x *FieldTypePhoneConfig) GetAllowCountries() []string {
	if x != nil {
		return x.AllowCountries
	}
	return nil
}

func (x *FieldTypePhoneConfig) GetExcludeCountries() []string {
	if x != nil {
		return x.ExcludeCountries
	}
	return nil
}

var File_field_type_phone_config_proto protoreflect.FileDescriptor

var file_field_type_phone_config_proto_rawDesc = []byte{
	0x0a, 0x1d, 0x66, 0x69, 0x65, 0x6c, 0x64, 0x5f, 0x74, 0x79, 0x70, 0x65, 0x5f, 0x70, 0x68, 0x6f,
	0x6e, 0x65, 0x5f, 0x63, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12,
	0x03, 0x6e, 0x65, 0x6d, 0x22, 0x6c, 0x0a, 0x14, 0x46, 0x69, 0x65, 0x6c, 0x64, 0x54, 0x79, 0x70,
	0x65, 0x50, 0x68, 0x6f, 0x6e, 0x65, 0x43, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x12, 0x27, 0x0a, 0x0f,
	0x61, 0x6c, 0x6c, 0x6f, 0x77, 0x5f, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x72, 0x69, 0x65, 0x73, 0x18,
	0x01, 0x20, 0x03, 0x28, 0x09, 0x52, 0x0e, 0x61, 0x6c, 0x6c, 0x6f, 0x77, 0x43, 0x6f, 0x75, 0x6e,
	0x74, 0x72, 0x69, 0x65, 0x73, 0x12, 0x2b, 0x0a, 0x11, 0x65, 0x78, 0x63, 0x6c, 0x75, 0x64, 0x65,
	0x5f, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x72, 0x69, 0x65, 0x73, 0x18, 0x02, 0x20, 0x03, 0x28, 0x09,
	0x52, 0x10, 0x65, 0x78, 0x63, 0x6c, 0x75, 0x64, 0x65, 0x43, 0x6f, 0x75, 0x6e, 0x74, 0x72, 0x69,
	0x65, 0x73, 0x42, 0x2a, 0x0a, 0x03, 0x6e, 0x65, 0x6d, 0x42, 0x14, 0x46, 0x69, 0x65, 0x6c, 0x64,
	0x54, 0x79, 0x70, 0x65, 0x50, 0x68, 0x6f, 0x6e, 0x65, 0x43, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x50,
	0x01, 0x5a, 0x0b, 0x6e, 0x65, 0x6d, 0x2f, 0x69, 0x64, 0x6c, 0x2f, 0x67, 0x65, 0x6e, 0x62, 0x06,
	0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_field_type_phone_config_proto_rawDescOnce sync.Once
	file_field_type_phone_config_proto_rawDescData = file_field_type_phone_config_proto_rawDesc
)

func file_field_type_phone_config_proto_rawDescGZIP() []byte {
	file_field_type_phone_config_proto_rawDescOnce.Do(func() {
		file_field_type_phone_config_proto_rawDescData = protoimpl.X.CompressGZIP(file_field_type_phone_config_proto_rawDescData)
	})
	return file_field_type_phone_config_proto_rawDescData
}

var file_field_type_phone_config_proto_msgTypes = make([]protoimpl.MessageInfo, 1)
var file_field_type_phone_config_proto_goTypes = []any{
	(*FieldTypePhoneConfig)(nil), // 0: nem.FieldTypePhoneConfig
}
var file_field_type_phone_config_proto_depIdxs = []int32{
	0, // [0:0] is the sub-list for method output_type
	0, // [0:0] is the sub-list for method input_type
	0, // [0:0] is the sub-list for extension type_name
	0, // [0:0] is the sub-list for extension extendee
	0, // [0:0] is the sub-list for field type_name
}

func init() { file_field_type_phone_config_proto_init() }
func file_field_type_phone_config_proto_init() {
	if File_field_type_phone_config_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_field_type_phone_config_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   1,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_field_type_phone_config_proto_goTypes,
		DependencyIndexes: file_field_type_phone_config_proto_depIdxs,
		MessageInfos:      file_field_type_phone_config_proto_msgTypes,
	}.Build()
	File_field_type_phone_config_proto = out.File
	file_field_type_phone_config_proto_rawDesc = nil
	file_field_type_phone_config_proto_goTypes = nil
	file_field_type_phone_config_proto_depIdxs = nil
}