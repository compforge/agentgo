package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// ReadResult describes the accepted file prefix. A missing run_close or
// agent_end is incomplete evidence; readers must not infer success from EOF.
type ReadResult struct {
	SessionID     string
	Records       uint64
	TruncatedTail bool
}

// Read streams records without retaining the transcript. Complete corrupt lines,
// unknown format versions, mixed sessions and sequence gaps are errors. Only an
// unterminated final line is ignored, including a valid JSON object without its
// terminating newline. The callback may decode Record.Data for the record type.
func Read(reader io.Reader, visit func(Record) error) (ReadResult, error) {
	result, _, err := read(reader, visit)
	return result, err
}

func read(reader io.Reader, visit func(Record) error) (ReadResult, int64, error) {
	var result ReadResult
	var offset int64
	r := bufio.NewReader(reader)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return result, offset, err
		}
		if err == io.EOF {
			result.TruncatedTail = len(line) != 0
			if result.Records == 0 {
				return result, offset, fmt.Errorf("session: missing complete header")
			}
			return result, offset, nil
		}
		var record Record
		if !utf8.Valid(line) || json.Unmarshal(line, &record) != nil || bytes.Equal(bytes.TrimSpace(line), []byte("null")) {
			return result, offset, fmt.Errorf("session line %d: invalid record", result.Records+1)
		}
		if record.Seq != result.Records+1 || record.SessionID == "" || record.Type == "" || record.Timestamp.IsZero() {
			return result, offset, fmt.Errorf("session line %d: invalid envelope", result.Records+1)
		}
		if result.Records == 0 {
			var header struct {
				Version int `json:"version"`
			}
			if record.Type != SessionStart || record.RunID != "" || json.Unmarshal(record.Data, &header) != nil || header.Version != FormatVersion {
				return result, offset, fmt.Errorf("session: invalid or unsupported header")
			}
			result.SessionID = record.SessionID
		} else if record.SessionID != result.SessionID || record.Type == SessionStart || record.RunID == "" {
			return result, offset, fmt.Errorf("session line %d: invalid session/run identity", record.Seq)
		}
		if visit != nil {
			if err := visit(record); err != nil {
				return result, offset, err
			}
		}
		result.Records = record.Seq
		offset += int64(len(line))
	}
}
