package tts

import "testing"

// Vectors from the reference implementation, rany2/edge-tts 7.3.1:
//
//	from edge_tts.drm import generate_sec_ms_gec_v2
//	generate_sec_ms_gec_v2(unix_timestamp)
//
// If Microsoft rotates the token again, regenerate these from the
// release that fixes it.
func TestEdgeTokenMatchesReference(t *testing.T) {
	for _, tc := range []struct {
		unix int64
		want string
	}{
		{0, "52BC7A17B50DFEC9-28C4394775C8751CD2F6859C030C06DB1489ACAB3B3A7651831FC26895177ED6"},
		{1700000000, "2CD23A2CA0B2595A-F3C846C370E05F9CC83ACC5F995C5F5D87A4D0F14CE8936A0F902E64099E38A2"},
		{1761649244, "327FA06BA89F249B-BD3F290D296F62FC467ECE0B11799C1DEEF260ADDF209D52D3BC878F0FBAB65A"},
		// Last second of the same five-minute window: same token.
		{1761649499, "327FA06BA89F249B-BD3F290D296F62FC467ECE0B11799C1DEEF260ADDF209D52D3BC878F0FBAB65A"},
		{1761649500, "165B493C37E561DB-1FD2AC43BE63BC724387E4ED8393A6641354A54F2BA8E2B6E54A12F60027BFAC"},
		{1893456000, "7442B6E630CDF0DE-CE82EC1509335DC856270C1EEB0F7F2575909AC26840D2C15B28A41EBB7E9CCF"},
	} {
		if got := edgeToken(tc.unix); got != tc.want {
			t.Errorf("edgeToken(%d) = %s, want %s", tc.unix, got, tc.want)
		}
	}
}
