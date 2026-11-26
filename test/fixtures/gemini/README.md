# Gemini fixtures

Every file here is **handcrafted**, not recorded: written from Google's
generateContent reference, the thought-signatures, function-calling,
long-decode and error pages (checked 2026-11-21), and the SSE framing of
Google's Go SDK parser. The ids, thought signatures and token counts are
made up. `stream_text.sse` uses CRLF framing and ends without a blank line
after its last event, to exercise both readings of the framing.

The fake (`test/fakegemini`) encodes some unverified assumptions, so
tests that pass against it pass whatever Google does in these cases:

- it accepts consecutive contents of the same role (the adapter sends tool
  results and the user text after them as two user contents);
- it rejects function responses mixed with other parts in one user
  content, which is the adapter's choice, not a documented Google rule;
- it bounds neither `functionCall.id` nor `functionResponse.id` in length,
  and Google documents no limit.

`make record-fixtures` (with `GEMINI_API_KEY`) writes real responses to
`recorded/` beside these, for comparison. It never writes the key or the
request headers.
