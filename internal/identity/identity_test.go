package identity

import "testing"

func TestLinkedAccountsShareAPerson(t *testing.T) {
	d, err := Load([]string{"IDENTITY_JAKE=discord:264301820258680834, email:Jake@Example.com", "OTHER=1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ conn, id, want string }{
		{"discord", "264301820258680834", "jake"},
		{"email", "jake@example.com", "jake"},
		{"discord", "999", "discord:999"},
		{"discord", "", ""},
	} {
		if got := d.Person(c.conn, c.id); got != c.want {
			t.Errorf("Person(%s, %s) = %q, want %q", c.conn, c.id, got, c.want)
		}
	}
}

func TestAccountClaimedTwiceIsRejected(t *testing.T) {
	if _, err := Load([]string{"IDENTITY_A=discord:1", "IDENTITY_B=discord:1"}); err == nil {
		t.Fatal("shared account accepted")
	}
}
