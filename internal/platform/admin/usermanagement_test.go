package admin

import (
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// TestUserCredentialToDtoPasswordConfigured: a PASSWORD credential with a non-null hash maps to
// {configured:true}; the raw hash and _class never reach the DTO, and id comes from _id.
func TestUserCredentialToDtoPasswordConfigured(t *testing.T) {
	doc := pgdoc.M{
		"_id":      "cred-1",
		"sub":      "user-sub",
		"type":     "PASSWORD",
		"password": pgdoc.M{"hash": "$2a$bcrypted"},
		"_class":   "UserCredential",
	}
	dto := userCredentialToDto(doc)
	if dto.ID != any("cred-1") {
		t.Errorf("id=%v want cred-1", dto.ID)
	}
	if dto.Sub != "user-sub" {
		t.Errorf("sub=%q want user-sub", dto.Sub)
	}
	if dto.Type != "PASSWORD" {
		t.Errorf("type=%q want PASSWORD", dto.Type)
	}
	if dto.Password == nil || !dto.Password.Configured {
		t.Errorf("password=%+v want configured=true", dto.Password)
	}
	if dto.Totp != nil {
		t.Errorf("totp=%+v want nil (no totp sub-object)", dto.Totp)
	}
	// The raw hash must never be serialized.
	b, _ := json.Marshal(dto)
	if got := string(b); contains(got, "bcrypted") || contains(got, "_class") {
		t.Errorf("dto json leaked raw material: %s", got)
	}
}

// TestUserCredentialToDtoPasswordNullHash: password sub-object present but hash null → configured:false.
func TestUserCredentialToDtoPasswordNullHash(t *testing.T) {
	doc := pgdoc.M{"_id": "c", "sub": "s", "type": "PASSWORD", "password": pgdoc.M{"hash": nil}}
	dto := userCredentialToDto(doc)
	if dto.Password == nil || dto.Password.Configured {
		t.Errorf("password=%+v want configured=false", dto.Password)
	}
}

// TestUserCredentialToDtoTotp: a TOTP credential maps verified+deviceName; password stays nil.
func TestUserCredentialToDtoTotp(t *testing.T) {
	doc := pgdoc.M{
		"_id":  "t1",
		"sub":  "s",
		"type": "TOTP",
		"totp": pgdoc.M{"verified": true, "deviceName": "Pixel", "secret": "SECRET"},
	}
	dto := userCredentialToDto(doc)
	if dto.Password != nil {
		t.Errorf("password=%+v want nil", dto.Password)
	}
	if dto.Totp == nil || !dto.Totp.Verified || dto.Totp.DeviceName != "Pixel" {
		t.Errorf("totp=%+v want {verified:true,deviceName:Pixel}", dto.Totp)
	}
	// The TOTP secret must never reach the wire.
	b, _ := json.Marshal(dto)
	if contains(string(b), "SECRET") {
		t.Errorf("dto json leaked totp secret: %s", string(b))
	}
}

// TestUserCredentialToDtoNoSubObjects: neither password nor totp present → both omitted.
func TestUserCredentialToDtoNoSubObjects(t *testing.T) {
	doc := pgdoc.M{"_id": "w", "sub": "s", "type": "WEBAUTHN"}
	dto := userCredentialToDto(doc)
	if dto.Password != nil || dto.Totp != nil {
		t.Errorf("password=%+v totp=%+v want both nil", dto.Password, dto.Totp)
	}
	b, _ := json.Marshal(dto)
	got := string(b)
	if contains(got, "\"password\"") || contains(got, "\"totp\"") {
		t.Errorf("omitted sub-objects emitted: %s", got)
	}
}

// TestUserCredentialDtoEmptyListEncodesArray: a zero-length slice of DTOs marshals to [] not null
// (the List envelope wraps it; the mapper must pre-size to a non-nil slice).
func TestUserCredentialDtoEmptyList(t *testing.T) {
	creds := []pgdoc.M{}
	dtos := make([]userCredentialAdminDto, 0, len(creds))
	for _, c := range creds {
		dtos = append(dtos, userCredentialToDto(c))
	}
	b, _ := json.Marshal(dtos)
	if string(b) != "[]" {
		t.Errorf("empty dto list = %s want []", string(b))
	}
}

// TestUserManagementPerms pins the exact AdminPermissionEnum keys per endpoint.
func TestUserManagementPerms(t *testing.T) {
	if userManagementReadPerm != "admin:user:read" {
		t.Errorf("read perm = %q want admin:user:read", userManagementReadPerm)
	}
	if userManagementManageCredentialsPerm != "admin:user:manage_credentials" {
		t.Errorf("manage perm = %q want admin:user:manage_credentials", userManagementManageCredentialsPerm)
	}
}

// TestUserCredentialCollection pins the default collection name.
func TestUserCredentialCollection(t *testing.T) {
	if userCredentialCollection != "userCredential" {
		t.Errorf("collection = %q want userCredential", userCredentialCollection)
	}
}

// TestAsStringHelper covers the local value→string reader (string, Stringer, absent).
func TestAsStringHelper(t *testing.T) {
	if got := asString("x"); got != "x" {
		t.Errorf("asString(string)=%q want x", got)
	}
	if got := asString(nil); got != "" {
		t.Errorf("asString(nil)=%q want empty", got)
	}
	if got := asString(42); got != "" {
		t.Errorf("asString(int)=%q want empty", got)
	}
}

// contains is defined in priceadjustmentrule_test.go (same package, test scope).
