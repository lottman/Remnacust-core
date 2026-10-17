package quotausage

import "testing"

func TestCanonicalPrivateCounter(t *testing.T) {
	counter := Counter{Account: "ff29ee68-a625-4160-9573-cc17de317a5d", Owner: "9007199254740993", Factor: 1500000000, Scale: 1000000000, Direction: "uplink"}
	name, err := counter.Name()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(name)
	if err != nil || parsed != counter || !IsUsageCounter(name) {
		t.Fatal(parsed, err)
	}
	for _, invalid := range []string{
		"qfree>>>ff29ee68-a625-4160-9573-cc17de317a5d>>>01>>>1>>>100>>>traffic>>>uplink",
		"qfree>>>ff29ee68-a625-4160-9573-cc17de317a5d>>>1>>>0>>>100>>>traffic>>>uplink",
		"qfree>>>ff29ee68-a625-4160-9573-cc17de317a5d>>>1>>>10001>>>100>>>traffic>>>uplink",
		"qfree>>>FF29EE68-A625-4160-9573-CC17DE317A5D>>>1>>>1>>>100>>>traffic>>>uplink",
		"qfree>>>00000000-0000-0000-0000-000000000000>>>1>>>1>>>100>>>traffic>>>uplink",
		"qfree>>>ff29ee68-a625-4160-9573-cc17de317a5d>>>9223372036854775808>>>1>>>100>>>traffic>>>uplink",
		"qfree>>>ff29ee68-a625-4160-9573-cc17de317a5d>>>1>>>1>>>99>>>traffic>>>uplink",
		"qfree>>>ff29ee68-a625-4160-9573-cc17de317a5d>>>1>>>1>>>100>>>traffic>>>sideways",
	} {
		if _, err := Parse(invalid); err == nil || IsUsageCounter(invalid) {
			t.Fatalf("accepted %q", invalid)
		}
	}
	if !IsUsageCounter("user>>>901>>>traffic>>>downlink") || IsUsageCounter("user>>>>>>traffic>>>uplink") {
		t.Fatal("invalid regular counter predicate")
	}
}
