// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.35.2
// 	protoc        v5.28.3
// source: review_config.proto

package gen

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type ReviewConfigStatus int32

const (
	ReviewConfigStatus_REVIEW_CONFIG_STATUS_INVALID  ReviewConfigStatus = 0
	ReviewConfigStatus_REVIEW_CONFIG_STATUS_ACTIVE   ReviewConfigStatus = 1
	ReviewConfigStatus_REVIEW_CONFIG_STATUS_DISABLED ReviewConfigStatus = 2
)

// Enum value maps for ReviewConfigStatus.
var (
	ReviewConfigStatus_name = map[int32]string{
		0: "REVIEW_CONFIG_STATUS_INVALID",
		1: "REVIEW_CONFIG_STATUS_ACTIVE",
		2: "REVIEW_CONFIG_STATUS_DISABLED",
	}
	ReviewConfigStatus_value = map[string]int32{
		"REVIEW_CONFIG_STATUS_INVALID":  0,
		"REVIEW_CONFIG_STATUS_ACTIVE":   1,
		"REVIEW_CONFIG_STATUS_DISABLED": 2,
	}
)

func (x ReviewConfigStatus) Enum() *ReviewConfigStatus {
	p := new(ReviewConfigStatus)
	*p = x
	return p
}

func (x ReviewConfigStatus) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (ReviewConfigStatus) Descriptor() protoreflect.EnumDescriptor {
	return file_review_config_proto_enumTypes[0].Descriptor()
}

func (ReviewConfigStatus) Type() protoreflect.EnumType {
	return &file_review_config_proto_enumTypes[0]
}

func (x ReviewConfigStatus) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use ReviewConfigStatus.Descriptor instead.
func (ReviewConfigStatus) EnumDescriptor() ([]byte, []int) {
	return file_review_config_proto_rawDescGZIP(), []int{0}
}

type ReviewConfigType int32

const (
	ReviewConfigType_REVIEW_CONFIG_TYPE_INVALID           ReviewConfigType = 0
	ReviewConfigType_REVIEW_CONFIG_TYPE_DEFAULT           ReviewConfigType = 1
	ReviewConfigType_REVIEW_CONFIG_TYPE_PROJECT_VERSION   ReviewConfigType = 2
	ReviewConfigType_REVIEW_CONFIG_TYPE_EXTENSION_VERSION ReviewConfigType = 3
)

// Enum value maps for ReviewConfigType.
var (
	ReviewConfigType_name = map[int32]string{
		0: "REVIEW_CONFIG_TYPE_INVALID",
		1: "REVIEW_CONFIG_TYPE_DEFAULT",
		2: "REVIEW_CONFIG_TYPE_PROJECT_VERSION",
		3: "REVIEW_CONFIG_TYPE_EXTENSION_VERSION",
	}
	ReviewConfigType_value = map[string]int32{
		"REVIEW_CONFIG_TYPE_INVALID":           0,
		"REVIEW_CONFIG_TYPE_DEFAULT":           1,
		"REVIEW_CONFIG_TYPE_PROJECT_VERSION":   2,
		"REVIEW_CONFIG_TYPE_EXTENSION_VERSION": 3,
	}
)

func (x ReviewConfigType) Enum() *ReviewConfigType {
	p := new(ReviewConfigType)
	*p = x
	return p
}

func (x ReviewConfigType) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (ReviewConfigType) Descriptor() protoreflect.EnumDescriptor {
	return file_review_config_proto_enumTypes[1].Descriptor()
}

func (ReviewConfigType) Type() protoreflect.EnumType {
	return &file_review_config_proto_enumTypes[1]
}

func (x ReviewConfigType) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use ReviewConfigType.Descriptor instead.
func (ReviewConfigType) EnumDescriptor() ([]byte, []int) {
	return file_review_config_proto_rawDescGZIP(), []int{1}
}

type ReviewConfigUserRole int32

const (
	ReviewConfigUserRole_REVIEW_CONFIG_USER_ROLE_INVALID      ReviewConfigUserRole = 0
	ReviewConfigUserRole_REVIEW_CONFIG_USER_ROLE_ADMIN        ReviewConfigUserRole = 1
	ReviewConfigUserRole_REVIEW_CONFIG_USER_ROLE_DEVELOPER    ReviewConfigUserRole = 2
	ReviewConfigUserRole_REVIEW_CONFIG_USER_ROLE_DATA_MANAGER ReviewConfigUserRole = 3
	ReviewConfigUserRole_REVIEW_CONFIG_USER_ROLE_DATA_ANALYST ReviewConfigUserRole = 4
	ReviewConfigUserRole_REVIEW_CONFIG_USER_ROLE_VIEWER       ReviewConfigUserRole = 5
)

// Enum value maps for ReviewConfigUserRole.
var (
	ReviewConfigUserRole_name = map[int32]string{
		0: "REVIEW_CONFIG_USER_ROLE_INVALID",
		1: "REVIEW_CONFIG_USER_ROLE_ADMIN",
		2: "REVIEW_CONFIG_USER_ROLE_DEVELOPER",
		3: "REVIEW_CONFIG_USER_ROLE_DATA_MANAGER",
		4: "REVIEW_CONFIG_USER_ROLE_DATA_ANALYST",
		5: "REVIEW_CONFIG_USER_ROLE_VIEWER",
	}
	ReviewConfigUserRole_value = map[string]int32{
		"REVIEW_CONFIG_USER_ROLE_INVALID":      0,
		"REVIEW_CONFIG_USER_ROLE_ADMIN":        1,
		"REVIEW_CONFIG_USER_ROLE_DEVELOPER":    2,
		"REVIEW_CONFIG_USER_ROLE_DATA_MANAGER": 3,
		"REVIEW_CONFIG_USER_ROLE_DATA_ANALYST": 4,
		"REVIEW_CONFIG_USER_ROLE_VIEWER":       5,
	}
)

func (x ReviewConfigUserRole) Enum() *ReviewConfigUserRole {
	p := new(ReviewConfigUserRole)
	*p = x
	return p
}

func (x ReviewConfigUserRole) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (ReviewConfigUserRole) Descriptor() protoreflect.EnumDescriptor {
	return file_review_config_proto_enumTypes[2].Descriptor()
}

func (ReviewConfigUserRole) Type() protoreflect.EnumType {
	return &file_review_config_proto_enumTypes[2]
}

func (x ReviewConfigUserRole) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use ReviewConfigUserRole.Descriptor instead.
func (ReviewConfigUserRole) EnumDescriptor() ([]byte, []int) {
	return file_review_config_proto_rawDescGZIP(), []int{2}
}

type ReviewConfig struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Uuid          string                 `protobuf:"bytes,1,opt,name=uuid,proto3" json:"uuid,omitempty"`
	Types         []ReviewConfigType     `protobuf:"varint,2,rep,packed,name=types,proto3,enum=nem.ReviewConfigType" json:"types,omitempty"`
	UserRoles     []ReviewConfigUserRole `protobuf:"varint,3,rep,packed,name=user_roles,json=userRoles,proto3,enum=nem.ReviewConfigUserRole" json:"user_roles,omitempty"`
	MinReviews    int64                  `protobuf:"varint,4,opt,name=min_reviews,json=minReviews,proto3" json:"min_reviews,omitempty"`
	Status        ReviewConfigStatus     `protobuf:"varint,5,opt,name=status,proto3,enum=nem.ReviewConfigStatus" json:"status,omitempty"`
	CreatedAt     *timestamppb.Timestamp `protobuf:"bytes,6,opt,name=created_at,json=createdAt,proto3" json:"created_at,omitempty"`
	UpdatedAt     *timestamppb.Timestamp `protobuf:"bytes,7,opt,name=updated_at,json=updatedAt,proto3" json:"updated_at,omitempty"`
	CreatedByUuid string                 `protobuf:"bytes,8,opt,name=created_by_uuid,json=createdByUuid,proto3" json:"created_by_uuid,omitempty"`
	UpdatedByUuid string                 `protobuf:"bytes,9,opt,name=updated_by_uuid,json=updatedByUuid,proto3" json:"updated_by_uuid,omitempty"`
}

func (x *ReviewConfig) Reset() {
	*x = ReviewConfig{}
	mi := &file_review_config_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ReviewConfig) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ReviewConfig) ProtoMessage() {}

func (x *ReviewConfig) ProtoReflect() protoreflect.Message {
	mi := &file_review_config_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ReviewConfig.ProtoReflect.Descriptor instead.
func (*ReviewConfig) Descriptor() ([]byte, []int) {
	return file_review_config_proto_rawDescGZIP(), []int{0}
}

func (x *ReviewConfig) GetUuid() string {
	if x != nil {
		return x.Uuid
	}
	return ""
}

func (x *ReviewConfig) GetTypes() []ReviewConfigType {
	if x != nil {
		return x.Types
	}
	return nil
}

func (x *ReviewConfig) GetUserRoles() []ReviewConfigUserRole {
	if x != nil {
		return x.UserRoles
	}
	return nil
}

func (x *ReviewConfig) GetMinReviews() int64 {
	if x != nil {
		return x.MinReviews
	}
	return 0
}

func (x *ReviewConfig) GetStatus() ReviewConfigStatus {
	if x != nil {
		return x.Status
	}
	return ReviewConfigStatus_REVIEW_CONFIG_STATUS_INVALID
}

func (x *ReviewConfig) GetCreatedAt() *timestamppb.Timestamp {
	if x != nil {
		return x.CreatedAt
	}
	return nil
}

func (x *ReviewConfig) GetUpdatedAt() *timestamppb.Timestamp {
	if x != nil {
		return x.UpdatedAt
	}
	return nil
}

func (x *ReviewConfig) GetCreatedByUuid() string {
	if x != nil {
		return x.CreatedByUuid
	}
	return ""
}

func (x *ReviewConfig) GetUpdatedByUuid() string {
	if x != nil {
		return x.UpdatedByUuid
	}
	return ""
}

var File_review_config_proto protoreflect.FileDescriptor

var file_review_config_proto_rawDesc = []byte{
	0x0a, 0x13, 0x72, 0x65, 0x76, 0x69, 0x65, 0x77, 0x5f, 0x63, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x2e,
	0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x03, 0x6e, 0x65, 0x6d, 0x1a, 0x1f, 0x67, 0x6f, 0x6f, 0x67,
	0x6c, 0x65, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2f, 0x74, 0x69, 0x6d, 0x65,
	0x73, 0x74, 0x61, 0x6d, 0x70, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22, 0xa1, 0x03, 0x0a, 0x0c,
	0x52, 0x65, 0x76, 0x69, 0x65, 0x77, 0x43, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x12, 0x12, 0x0a, 0x04,
	0x75, 0x75, 0x69, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x75, 0x75, 0x69, 0x64,
	0x12, 0x2b, 0x0a, 0x05, 0x74, 0x79, 0x70, 0x65, 0x73, 0x18, 0x02, 0x20, 0x03, 0x28, 0x0e, 0x32,
	0x15, 0x2e, 0x6e, 0x65, 0x6d, 0x2e, 0x52, 0x65, 0x76, 0x69, 0x65, 0x77, 0x43, 0x6f, 0x6e, 0x66,
	0x69, 0x67, 0x54, 0x79, 0x70, 0x65, 0x52, 0x05, 0x74, 0x79, 0x70, 0x65, 0x73, 0x12, 0x38, 0x0a,
	0x0a, 0x75, 0x73, 0x65, 0x72, 0x5f, 0x72, 0x6f, 0x6c, 0x65, 0x73, 0x18, 0x03, 0x20, 0x03, 0x28,
	0x0e, 0x32, 0x19, 0x2e, 0x6e, 0x65, 0x6d, 0x2e, 0x52, 0x65, 0x76, 0x69, 0x65, 0x77, 0x43, 0x6f,
	0x6e, 0x66, 0x69, 0x67, 0x55, 0x73, 0x65, 0x72, 0x52, 0x6f, 0x6c, 0x65, 0x52, 0x09, 0x75, 0x73,
	0x65, 0x72, 0x52, 0x6f, 0x6c, 0x65, 0x73, 0x12, 0x1f, 0x0a, 0x0b, 0x6d, 0x69, 0x6e, 0x5f, 0x72,
	0x65, 0x76, 0x69, 0x65, 0x77, 0x73, 0x18, 0x04, 0x20, 0x01, 0x28, 0x03, 0x52, 0x0a, 0x6d, 0x69,
	0x6e, 0x52, 0x65, 0x76, 0x69, 0x65, 0x77, 0x73, 0x12, 0x2f, 0x0a, 0x06, 0x73, 0x74, 0x61, 0x74,
	0x75, 0x73, 0x18, 0x05, 0x20, 0x01, 0x28, 0x0e, 0x32, 0x17, 0x2e, 0x6e, 0x65, 0x6d, 0x2e, 0x52,
	0x65, 0x76, 0x69, 0x65, 0x77, 0x43, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x53, 0x74, 0x61, 0x74, 0x75,
	0x73, 0x52, 0x06, 0x73, 0x74, 0x61, 0x74, 0x75, 0x73, 0x12, 0x39, 0x0a, 0x0a, 0x63, 0x72, 0x65,
	0x61, 0x74, 0x65, 0x64, 0x5f, 0x61, 0x74, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x1a, 0x2e,
	0x67, 0x6f, 0x6f, 0x67, 0x6c, 0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2e,
	0x54, 0x69, 0x6d, 0x65, 0x73, 0x74, 0x61, 0x6d, 0x70, 0x52, 0x09, 0x63, 0x72, 0x65, 0x61, 0x74,
	0x65, 0x64, 0x41, 0x74, 0x12, 0x39, 0x0a, 0x0a, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x64, 0x5f,
	0x61, 0x74, 0x18, 0x07, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x1a, 0x2e, 0x67, 0x6f, 0x6f, 0x67, 0x6c,
	0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2e, 0x54, 0x69, 0x6d, 0x65, 0x73,
	0x74, 0x61, 0x6d, 0x70, 0x52, 0x09, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x64, 0x41, 0x74, 0x12,
	0x26, 0x0a, 0x0f, 0x63, 0x72, 0x65, 0x61, 0x74, 0x65, 0x64, 0x5f, 0x62, 0x79, 0x5f, 0x75, 0x75,
	0x69, 0x64, 0x18, 0x08, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0d, 0x63, 0x72, 0x65, 0x61, 0x74, 0x65,
	0x64, 0x42, 0x79, 0x55, 0x75, 0x69, 0x64, 0x12, 0x26, 0x0a, 0x0f, 0x75, 0x70, 0x64, 0x61, 0x74,
	0x65, 0x64, 0x5f, 0x62, 0x79, 0x5f, 0x75, 0x75, 0x69, 0x64, 0x18, 0x09, 0x20, 0x01, 0x28, 0x09,
	0x52, 0x0d, 0x75, 0x70, 0x64, 0x61, 0x74, 0x65, 0x64, 0x42, 0x79, 0x55, 0x75, 0x69, 0x64, 0x2a,
	0x7a, 0x0a, 0x12, 0x52, 0x65, 0x76, 0x69, 0x65, 0x77, 0x43, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x53,
	0x74, 0x61, 0x74, 0x75, 0x73, 0x12, 0x20, 0x0a, 0x1c, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f,
	0x43, 0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f, 0x53, 0x54, 0x41, 0x54, 0x55, 0x53, 0x5f, 0x49, 0x4e,
	0x56, 0x41, 0x4c, 0x49, 0x44, 0x10, 0x00, 0x12, 0x1f, 0x0a, 0x1b, 0x52, 0x45, 0x56, 0x49, 0x45,
	0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f, 0x53, 0x54, 0x41, 0x54, 0x55, 0x53, 0x5f,
	0x41, 0x43, 0x54, 0x49, 0x56, 0x45, 0x10, 0x01, 0x12, 0x21, 0x0a, 0x1d, 0x52, 0x45, 0x56, 0x49,
	0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f, 0x53, 0x54, 0x41, 0x54, 0x55, 0x53,
	0x5f, 0x44, 0x49, 0x53, 0x41, 0x42, 0x4c, 0x45, 0x44, 0x10, 0x02, 0x2a, 0xa4, 0x01, 0x0a, 0x10,
	0x52, 0x65, 0x76, 0x69, 0x65, 0x77, 0x43, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x54, 0x79, 0x70, 0x65,
	0x12, 0x1e, 0x0a, 0x1a, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49,
	0x47, 0x5f, 0x54, 0x59, 0x50, 0x45, 0x5f, 0x49, 0x4e, 0x56, 0x41, 0x4c, 0x49, 0x44, 0x10, 0x00,
	0x12, 0x1e, 0x0a, 0x1a, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49,
	0x47, 0x5f, 0x54, 0x59, 0x50, 0x45, 0x5f, 0x44, 0x45, 0x46, 0x41, 0x55, 0x4c, 0x54, 0x10, 0x01,
	0x12, 0x26, 0x0a, 0x22, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49,
	0x47, 0x5f, 0x54, 0x59, 0x50, 0x45, 0x5f, 0x50, 0x52, 0x4f, 0x4a, 0x45, 0x43, 0x54, 0x5f, 0x56,
	0x45, 0x52, 0x53, 0x49, 0x4f, 0x4e, 0x10, 0x02, 0x12, 0x28, 0x0a, 0x24, 0x52, 0x45, 0x56, 0x49,
	0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f, 0x54, 0x59, 0x50, 0x45, 0x5f, 0x45,
	0x58, 0x54, 0x45, 0x4e, 0x53, 0x49, 0x4f, 0x4e, 0x5f, 0x56, 0x45, 0x52, 0x53, 0x49, 0x4f, 0x4e,
	0x10, 0x03, 0x2a, 0xfd, 0x01, 0x0a, 0x14, 0x52, 0x65, 0x76, 0x69, 0x65, 0x77, 0x43, 0x6f, 0x6e,
	0x66, 0x69, 0x67, 0x55, 0x73, 0x65, 0x72, 0x52, 0x6f, 0x6c, 0x65, 0x12, 0x23, 0x0a, 0x1f, 0x52,
	0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f, 0x55, 0x53, 0x45,
	0x52, 0x5f, 0x52, 0x4f, 0x4c, 0x45, 0x5f, 0x49, 0x4e, 0x56, 0x41, 0x4c, 0x49, 0x44, 0x10, 0x00,
	0x12, 0x21, 0x0a, 0x1d, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49,
	0x47, 0x5f, 0x55, 0x53, 0x45, 0x52, 0x5f, 0x52, 0x4f, 0x4c, 0x45, 0x5f, 0x41, 0x44, 0x4d, 0x49,
	0x4e, 0x10, 0x01, 0x12, 0x25, 0x0a, 0x21, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f,
	0x4e, 0x46, 0x49, 0x47, 0x5f, 0x55, 0x53, 0x45, 0x52, 0x5f, 0x52, 0x4f, 0x4c, 0x45, 0x5f, 0x44,
	0x45, 0x56, 0x45, 0x4c, 0x4f, 0x50, 0x45, 0x52, 0x10, 0x02, 0x12, 0x28, 0x0a, 0x24, 0x52, 0x45,
	0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f, 0x55, 0x53, 0x45, 0x52,
	0x5f, 0x52, 0x4f, 0x4c, 0x45, 0x5f, 0x44, 0x41, 0x54, 0x41, 0x5f, 0x4d, 0x41, 0x4e, 0x41, 0x47,
	0x45, 0x52, 0x10, 0x03, 0x12, 0x28, 0x0a, 0x24, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43,
	0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f, 0x55, 0x53, 0x45, 0x52, 0x5f, 0x52, 0x4f, 0x4c, 0x45, 0x5f,
	0x44, 0x41, 0x54, 0x41, 0x5f, 0x41, 0x4e, 0x41, 0x4c, 0x59, 0x53, 0x54, 0x10, 0x04, 0x12, 0x22,
	0x0a, 0x1e, 0x52, 0x45, 0x56, 0x49, 0x45, 0x57, 0x5f, 0x43, 0x4f, 0x4e, 0x46, 0x49, 0x47, 0x5f,
	0x55, 0x53, 0x45, 0x52, 0x5f, 0x52, 0x4f, 0x4c, 0x45, 0x5f, 0x56, 0x49, 0x45, 0x57, 0x45, 0x52,
	0x10, 0x05, 0x42, 0x22, 0x0a, 0x03, 0x6e, 0x65, 0x6d, 0x42, 0x0c, 0x52, 0x65, 0x76, 0x69, 0x65,
	0x77, 0x43, 0x6f, 0x6e, 0x66, 0x69, 0x67, 0x50, 0x01, 0x5a, 0x0b, 0x6e, 0x65, 0x6d, 0x2f, 0x69,
	0x64, 0x6c, 0x2f, 0x67, 0x65, 0x6e, 0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_review_config_proto_rawDescOnce sync.Once
	file_review_config_proto_rawDescData = file_review_config_proto_rawDesc
)

func file_review_config_proto_rawDescGZIP() []byte {
	file_review_config_proto_rawDescOnce.Do(func() {
		file_review_config_proto_rawDescData = protoimpl.X.CompressGZIP(file_review_config_proto_rawDescData)
	})
	return file_review_config_proto_rawDescData
}

var file_review_config_proto_enumTypes = make([]protoimpl.EnumInfo, 3)
var file_review_config_proto_msgTypes = make([]protoimpl.MessageInfo, 1)
var file_review_config_proto_goTypes = []any{
	(ReviewConfigStatus)(0),       // 0: nem.ReviewConfigStatus
	(ReviewConfigType)(0),         // 1: nem.ReviewConfigType
	(ReviewConfigUserRole)(0),     // 2: nem.ReviewConfigUserRole
	(*ReviewConfig)(nil),          // 3: nem.ReviewConfig
	(*timestamppb.Timestamp)(nil), // 4: google.protobuf.Timestamp
}
var file_review_config_proto_depIdxs = []int32{
	1, // 0: nem.ReviewConfig.types:type_name -> nem.ReviewConfigType
	2, // 1: nem.ReviewConfig.user_roles:type_name -> nem.ReviewConfigUserRole
	0, // 2: nem.ReviewConfig.status:type_name -> nem.ReviewConfigStatus
	4, // 3: nem.ReviewConfig.created_at:type_name -> google.protobuf.Timestamp
	4, // 4: nem.ReviewConfig.updated_at:type_name -> google.protobuf.Timestamp
	5, // [5:5] is the sub-list for method output_type
	5, // [5:5] is the sub-list for method input_type
	5, // [5:5] is the sub-list for extension type_name
	5, // [5:5] is the sub-list for extension extendee
	0, // [0:5] is the sub-list for field type_name
}

func init() { file_review_config_proto_init() }
func file_review_config_proto_init() {
	if File_review_config_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_review_config_proto_rawDesc,
			NumEnums:      3,
			NumMessages:   1,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_review_config_proto_goTypes,
		DependencyIndexes: file_review_config_proto_depIdxs,
		EnumInfos:         file_review_config_proto_enumTypes,
		MessageInfos:      file_review_config_proto_msgTypes,
	}.Build()
	File_review_config_proto = out.File
	file_review_config_proto_rawDesc = nil
	file_review_config_proto_goTypes = nil
	file_review_config_proto_depIdxs = nil
}
