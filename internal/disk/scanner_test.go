package disk

import "testing"

const sample = `{"blockdevices":[
  {"name":"sda","path":"/dev/sda","size":1000,"type":"disk","rota":true,"fstype":null,"mountpoint":null,"model":"HDD","children":[
    {"name":"sda1","path":"/dev/sda1","size":1000,"type":"part","mountpoint":"/"}
  ]},
  {"name":"sdb","path":"/dev/sdb","size":50000000000,"type":"disk","rota":false,"fstype":null,"mountpoint":null,"model":"SSD"},
  {"name":"sdc","path":"/dev/sdc","size":100,"type":"disk","rota":false,"fstype":"ext4","mountpoint":null,"model":"used"}
]}`

func TestParseAndFilterEmptyDisks(t *testing.T) {
	disks, err := parseAndFilter([]byte(sample), "node1", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// Only sdb is empty (sda has a partition child, sdc has a filesystem).
	if len(disks) != 1 {
		t.Fatalf("got %d disks want 1: %+v", len(disks), disks)
	}
	if disks[0].Path != "/dev/sdb" || disks[0].Node != "node1" {
		t.Fatalf("unexpected disk: %+v", disks[0])
	}
}

func TestMinSizeFilter(t *testing.T) {
	disks, err := parseAndFilter([]byte(sample), "n", Filter{MinSize: 60000000000})
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 0 {
		t.Fatalf("expected sdb filtered out by min size, got %+v", disks)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	disks, _ := parseAndFilter([]byte(sample), "n", Filter{})
	s, err := Encode(disks)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(disks) {
		t.Fatalf("roundtrip mismatch")
	}
}
