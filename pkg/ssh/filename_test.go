package ssh

import (
	"net"
	"testing"
)

func TestIPToFilename_IPv4(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"192.168.1.1", "192.168.1.1"},
		{"10.0.0.1", "10.0.0.1"},
		{"203.0.113.0", "203.0.113.0"},
		{"255.255.255.255", "255.255.255.255"},
		{"0.0.0.0", "0.0.0.0"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got := IPToFilename(c.input)
			if got != c.want {
				t.Fatalf("IPToFilename(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestIPToFilename_IPv6(t *testing.T) {
	cases := []struct {
		input    string
		wantNot  string
		contains string
	}{
		{"2001:0db8:85a3:0000:0000:8a2e:0370:7334", "invalid-ip", "2001"},
		{"::1", "invalid-ip", "0000"},
		{"::ffff:192.168.1.1", "invalid-ip", ""},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got := IPToFilename(c.input)
			if got == c.wantNot {
				t.Fatalf("IPToFilename(%q) = %q, did not expect %q", c.input, got, c.wantNot)
			}
			if c.contains != "" {
				found := false
				for i := 0; i <= len(got)-len(c.contains); i++ {
					if got[i:i+len(c.contains)] == c.contains {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("IPToFilename(%q) = %q, expected to contain %q", c.input, got, c.contains)
				}
			}
			// Filename should be filesystem-safe: no colons
			for _, r := range got {
				if r == ':' {
					t.Fatalf("IPToFilename(%q) = %q, contains colon which is not filesystem-safe", c.input, got)
				}
			}
		})
	}
}

func TestIPToFilename_Invalid(t *testing.T) {
	cases := []string{
		"invalid-ip",
		"not.an.ip",
		"",
		"999.999.999.999",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			got := IPToFilename(input)
			if got != "invalid-ip" {
				t.Fatalf("IPToFilename(%q) = %q, want %q", input, got, "invalid-ip")
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "hello", "hello"},
		{"with dots", "file.name", "file.name"},
		{"with dashes", "my-file", "my-file"},
		{"with underscores", "my_file", "my_file"},
		{"colons replaced", "10:20:30", "10-20-30"},
		{"multiple special chars collapse", "a::b", "a-b"},
		{"leading special stripped", "-hello", "hello"},
		{"trailing special stripped", "hello-", "hello"},
		{"leading dot stripped", ".hidden", "hidden"},
		{"empty input", "", "ip-address"},
		{"all special chars", ":::", "ip-address"},
		{"mixed ipv4", "192.168.1.1", "192.168.1.1"},
		{"ipv4 dots preserved", "10.0.0.1", "10.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeFilename(c.input)
			if got != c.want {
				t.Fatalf("sanitizeFilename(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestSanitizeZone(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "eth0", "eth0"},
		{"with percent", "eth%0", "eth-0"},
		{"alphanumeric", "wlan1", "wlan1"},
		{"special chars", "en/0/1", "en-0-1"},
		{"dashes preserved", "my-zone", "my-zone"},
		{"underscores preserved", "my_zone", "my_zone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeZone(c.input)
			if got != c.want {
				t.Fatalf("sanitizeZone(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestConvertIPv6(t *testing.T) {
	cases := []struct {
		name string
		ip   string
	}{
		{"full address", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
		{"loopback", "::1"},
		{"link-local", "fe80::1"},
		{"mapped v4", "::ffff:192.168.1.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed := net.ParseIP(c.ip)
			if parsed == nil {
				t.Fatalf("failed to parse IP %q", c.ip)
			}
			got := convertIPv6(parsed)
			if got == "invalid-ipv6" {
				t.Fatalf("convertIPv6(%q) returned invalid-ipv6", c.ip)
			}
			// Result should be filesystem-safe: no colons
			for _, r := range got {
				if r == ':' {
					t.Fatalf("convertIPv6(%q) = %q, contains colon", c.ip, got)
				}
			}
			// Result should contain dashes as separators
			if len(got) == 0 {
				t.Fatal("convertIPv6 returned empty string")
			}
		})
	}
}

func TestConvertIPv6_SpecificValues(t *testing.T) {
	// 2001:0db8:... => first 8 bytes are 20 01 0d b8 85 a3 00 00
	// Expected format: "%02x%02x-%02x%02x-%02x%02x-%02x%02x"
	// => "2001-0db8-85a3-0000"
	parsed := net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")
	got := convertIPv6(parsed)
	want := "2001-0db8-85a3-0000"
	if got != want {
		t.Fatalf("convertIPv6(2001:0db8:85a3::) = %q, want %q", got, want)
	}
}

func TestConvertIPv6_Loopback(t *testing.T) {
	// ::1 => 16 bytes: 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 01
	// First 8 bytes: 00 00 00 00 00 00 00 00
	// Expected: "0000-0000-0000-0000"
	parsed := net.ParseIP("::1")
	got := convertIPv6(parsed)
	want := "0000-0000-0000-0000"
	if got != want {
		t.Fatalf("convertIPv6(::1) = %q, want %q", got, want)
	}
}

func TestIPToFilename_Consistency(t *testing.T) {
	// Same input should always produce same output
	ip := "192.168.1.100"
	result1 := IPToFilename(ip)
	result2 := IPToFilename(ip)
	if result1 != result2 {
		t.Fatalf("IPToFilename is not consistent: %q vs %q", result1, result2)
	}
}

func TestIPToFilename_IPv4WithPort(t *testing.T) {
	// net.ParseIP does not accept host:port — these should all be "invalid-ip"
	cases := []string{
		"192.168.1.1:22",
		"10.0.0.1:8080",
		"127.0.0.1:0",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			got := IPToFilename(input)
			if got != "invalid-ip" {
				t.Fatalf("IPToFilename(%q) = %q, want %q (port should make it invalid)", input, got, "invalid-ip")
			}
		})
	}
}

func TestIPToFilename_IPv6WithBrackets(t *testing.T) {
	// Bracketed IPv6 (as used in URLs) is not valid for net.ParseIP
	cases := []string{
		"[::1]",
		"[2001:db8::1]",
		"[::1]:22",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			got := IPToFilename(input)
			if got != "invalid-ip" {
				t.Fatalf("IPToFilename(%q) = %q, want %q (bracketed IPv6 should be invalid)", input, got, "invalid-ip")
			}
		})
	}
}

func TestIPToFilename_Whitespace(t *testing.T) {
	// Leading/trailing whitespace should make the IP invalid
	cases := []string{
		" 192.168.1.1",
		"192.168.1.1 ",
		" 192.168.1.1 ",
		"\t10.0.0.1",
		"10.0.0.1\n",
	}
	for _, input := range cases {
		t.Run("whitespace", func(t *testing.T) {
			got := IPToFilename(input)
			if got != "invalid-ip" {
				t.Fatalf("IPToFilename(%q) = %q, want %q (whitespace should make it invalid)", input, got, "invalid-ip")
			}
		})
	}
}

func TestIPToFilename_IPv4MappedIPv6_ExactValue(t *testing.T) {
	// ::ffff:192.168.1.1 is IPv4-mapped IPv6. net.ParseIP parses it, but
	// To4() returns non-nil, so it follows the IPv4 path.
	got := IPToFilename("::ffff:192.168.1.1")
	// The parsed IP's String() for IPv4-mapped is "192.168.1.1"
	ip := net.ParseIP("::ffff:192.168.1.1")
	if ip == nil {
		t.Fatal("failed to parse ::ffff:192.168.1.1")
	}
	if ip.To4() != nil {
		// IPv4-mapped IPv6 has non-nil To4(), so IPToFilename uses the IPv4 path
		if got != ip.String() {
			t.Fatalf("IPToFilename(::ffff:192.168.1.1) = %q, want %q", got, ip.String())
		}
	} else {
		// Should not contain colons
		for _, r := range got {
			if r == ':' {
				t.Fatalf("IPToFilename(::ffff:192.168.1.1) = %q, contains colon", got)
			}
		}
	}
}

func TestIPToFilename_IPv6_FullExpansion(t *testing.T) {
	// Verify that different textual representations of the same IPv6 produce the same filename
	representations := []string{
		"2001:0db8:0000:0000:0000:0000:0000:0001",
		"2001:db8::1",
		"2001:0db8::0001",
	}
	results := make(map[string]bool)
	for _, r := range representations {
		got := IPToFilename(r)
		results[got] = true
	}
	if len(results) != 1 {
		t.Fatalf("different representations of same IPv6 produced different filenames: %v", results)
	}
}

func TestIPToFilename_SpecialInvalidInputs(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"null byte", "\x00"},
		{"unicode", "☁"},
		{"dots only", "..."},
		{"just a number", "42"},
		{"negative number", "-1"},
		{"hex string", "0xDEADBEEF"},
		{"cidr notation", "192.168.1.0/24"},
		{"double colon only", "::"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IPToFilename(c.input)
			// "::" is a valid IPv6 (all zeros), everything else should be invalid
			if c.input == "::" {
				if got == "invalid-ip" {
					t.Fatalf("IPToFilename(%q) = %q, but :: is a valid IPv6 address", c.input, got)
				}
			} else {
				if got != "invalid-ip" {
					t.Fatalf("IPToFilename(%q) = %q, want %q", c.input, got, "invalid-ip")
				}
			}
		})
	}
}

func TestIPToFilename_IPv6_AllZeros(t *testing.T) {
	// "::" is the all-zeros IPv6 address, valid
	got := IPToFilename("::")
	if got == "invalid-ip" {
		t.Fatal("IPToFilename(::) should not return invalid-ip")
	}
	// Should be filesystem-safe
	for _, r := range got {
		if r == ':' {
			t.Fatalf("IPToFilename(::) = %q, contains colon", got)
		}
	}
}

func TestIPToFilename_NoUnsafeChars(t *testing.T) {
	// Test a variety of IPs to ensure no unsafe filesystem characters
	ips := []string{
		"192.168.1.1",
		"10.0.0.1",
		"2001:db8::1",
		"fe80::1",
		"::1",
		"::ffff:127.0.0.1",
	}
	unsafeChars := []rune{'/', '\\', ':', '*', '?', '"', '<', '>', '|'}
	for _, ip := range ips {
		got := IPToFilename(ip)
		if got == "invalid-ip" {
			continue
		}
		for _, ch := range unsafeChars {
			for _, r := range got {
				if r == ch {
					t.Fatalf("IPToFilename(%q) = %q, contains unsafe char %q", ip, got, string(ch))
				}
			}
		}
	}
}
