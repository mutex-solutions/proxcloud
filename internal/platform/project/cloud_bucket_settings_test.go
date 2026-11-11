package project

import "testing"

func TestBucketNameErr(t *testing.T) {
	for _, ok := range []string{"abc", "my-bucket", "logs.2026", "a1-b2.c3", "pcp-b-0123456789abcdef"} {
		if bucketNameErr(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "ab", "My_Bucket", "my bucket", "-start", "end-", "a..b", "10.0.0.1", "UPPER", "x" + string(make([]byte, 63))} {
		if bucketNameErr(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGrantPermissionErr(t *testing.T) {
	for _, ok := range []string{"READ", "READ_WRITE", "FULL"} {
		if grantPermissionErr(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "read", "WRITE", "ADMIN"} {
		if grantPermissionErr(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
