package quote

import "testing"

// Security review C (2026-10-28): keys that differ only by case, repeated
// keys and anything but snake case are refused anywhere in the value.
func TestCheckKeys(t *testing.T) {
	for in, ok := range map[string]bool{
		`{"id":"x","amount_out":5,"spent":[{"amount":1,"payload":"ab"}],"user_inputs":[0]}`: true,
		`{"amount_out":99000000,"AMOUNT_OUT":1}`:                                            false,
		`{"Amount_Out":1}`:                                                                  false,
		`{"amount_out":1,"amount_out":2}`:                                                   false,
		`{"spent":[{"amount":1,"Amount":9}]}`:                                               false,
		`{"pairs":[{"from":"ARG","to":"UARG","rate":"0.1"}]}`:                               true,
		`{"a":1} {"b":2}`:                                                                   false,
		`[1,2,{"x-y":1}]`:                                                                   false,
	} {
		if err := CheckKeys([]byte(in)); (err == nil) != ok {
			t.Errorf("CheckKeys(%s) = %v, want ok=%v", in, err, ok)
		}
	}
}
