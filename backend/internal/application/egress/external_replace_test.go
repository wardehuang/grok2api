package egress

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

func TestNormalizeExternalProxyURLsRejectsEmptyAndInvalid(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input []string
	}{
		{name: "nil", input: nil},
		{name: "empty", input: []string{}},
		{name: "blank", input: []string{"  "}},
		{name: "no-scheme", input: []string{"1.2.3.4:8080"}},
		{name: "path", input: []string{"http://1.2.3.4:8080/proxy"}},
		{name: "duplicate", input: []string{"http://1.2.3.4:8080", "http://1.2.3.4:8080"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeExternalProxyURLs(test.input)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestNormalizeExternalProxyURLsAcceptsHTTPAndSOCKS(t *testing.T) {
	t.Parallel()
	got, err := normalizeExternalProxyURLs([]string{
		"  socks5://user:pass@10.0.0.1:1080 ",
		"http://10.0.0.2:8080",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"socks5://user:pass@10.0.0.1:1080", "http://10.0.0.2:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %#v, want %#v", got, want)
	}
}

func TestNormalizeExternalProxyURLsRejectsOverflow(t *testing.T) {
	t.Parallel()
	values := make([]string, maxExternalProxyURLs+1)
	for i := range values {
		values[i] = "http://10.0.0.1:" + strconv.Itoa(11000+i)
	}
	_, err := normalizeExternalProxyURLs(values)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestRoundRobinAccountGroupsDistributesInIndexOrder(t *testing.T) {
	t.Parallel()
	got := roundRobinAccountGroups([]uint64{10, 20, 30, 40}, []uint64{1, 2})
	if !reflect.DeepEqual(got[1], []uint64{10, 30}) {
		t.Fatalf("node 1 = %#v", got[1])
	}
	if !reflect.DeepEqual(got[2], []uint64{20, 40}) {
		t.Fatalf("node 2 = %#v", got[2])
	}
}
