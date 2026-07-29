package lanserve

import (
	"slices"
	"strings"
	"testing"
)

// The device id is what lets an app tell that the machine it found on the
// LAN and the destination it can reach through the tunnel are one device.
// Before this key there was no shared identifier between the two discovery
// paths, so clients matched on device name and folded two machines called
// "MacBook Pro" into a single entry.
func TestBonjourTXTPublishesDeviceID(t *testing.T) {
	txt := BonjourTXT("llama3.2", "0f9d2e5c-1a2b-4c3d-8e9f-000000000001")

	var got string
	for _, kv := range txt {
		if strings.HasPrefix(kv, "device_id=") {
			got = strings.TrimPrefix(kv, "device_id=")
		}
	}
	if got != "0f9d2e5c-1a2b-4c3d-8e9f-000000000001" {
		t.Fatalf("device_id not published, got TXT %v", txt)
	}

	// The keys SiloMac already parses must survive verbatim -- this record is
	// consumed by shipped app versions that predate the new key.
	for _, want := range []string{"model=llama3.2", "version=1.0", "capabilities=chat,stream,e2e", "protocol=1"} {
		if !slices.Contains(txt, want) {
			t.Errorf("existing TXT key %q disappeared; got %v", want, txt)
		}
	}
}

// An empty id must be absent, not published as "device_id=". A client
// grouping by the value would otherwise fold every node that failed to
// resolve an id into one device -- strictly worse than the name matching it
// replaces, because the collision would be silent and unbounded.
func TestBonjourTXTOmitsEmptyDeviceID(t *testing.T) {
	for _, kv := range BonjourTXT("llama3.2", "") {
		if strings.HasPrefix(kv, "device_id") {
			t.Fatalf("empty device id must be omitted entirely, got %q", kv)
		}
	}
}
