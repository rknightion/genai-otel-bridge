// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import "google.golang.org/protobuf/encoding/protowire"

// decodePartialSuccess extracts the partial_success reject count + error_message from a 2xx OTLP export
// response body (ExportMetricsServiceResponse / ExportLogsServiceResponse). [#80] The OTLP/HTTP spec's
// canonical way for a backend to reject PART of an accepted (2xx) batch is a partial_success sub-message:
//
//	ExportMetricsServiceResponse { partial_success = 1 { rejected_data_points = 1; error_message = 2 } }
//	ExportLogsServiceResponse    { partial_success = 1 { rejected_log_records  = 1; error_message = 2 } }
//
// Both messages place partial_success at field 1, whose field 1 is the reject count (varint) and field 2
// the error_message (string) — so ONE decoder serves both planes. Returns (0, "") for an empty/absent/
// malformed body: a garbled response must never manufacture a phantom reject. rejected==0 with a non-empty
// message is a spec "warning" (whole batch accepted) — the caller treats only rejected>0 as a real reject.
func decodePartialSuccess(body []byte) (rejected int64, msg string) {
	sub := fieldBytes(body, 1) // the partial_success sub-message
	if sub == nil {
		return 0, ""
	}
	for len(sub) > 0 {
		num, typ, tl := protowire.ConsumeTag(sub)
		if tl < 0 {
			return 0, "" // malformed
		}
		sub = sub[tl:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, vl := protowire.ConsumeVarint(sub)
			if vl < 0 {
				return 0, ""
			}
			rejected = int64(v)
			sub = sub[vl:]
		case num == 2 && typ == protowire.BytesType:
			b, vl := protowire.ConsumeBytes(sub)
			if vl < 0 {
				return 0, ""
			}
			msg = string(b)
			sub = sub[vl:]
		default:
			vl := protowire.ConsumeFieldValue(num, typ, sub)
			if vl < 0 {
				return 0, ""
			}
			sub = sub[vl:]
		}
	}
	return rejected, msg
}

// fieldBytes returns the last bytes-typed value of field num in a protobuf message (nil if absent/
// malformed). Used to reach the partial_success sub-message without importing collector/* types.
func fieldBytes(b []byte, num protowire.Number) []byte {
	var out []byte
	for len(b) > 0 {
		n, typ, tl := protowire.ConsumeTag(b)
		if tl < 0 {
			return out
		}
		b = b[tl:]
		if n == num && typ == protowire.BytesType {
			v, vl := protowire.ConsumeBytes(b)
			if vl < 0 {
				return out
			}
			out = v
			b = b[vl:]
			continue
		}
		vl := protowire.ConsumeFieldValue(n, typ, b)
		if vl < 0 {
			return out
		}
		b = b[vl:]
	}
	return out
}
