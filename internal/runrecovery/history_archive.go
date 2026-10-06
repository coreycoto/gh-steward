package runrecovery

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// These are archive-specific limits. Ordinary JSON files and checkpoints keep
// their existing 8 MiB limit; individual provider records cannot hide a blob.
const (
	MaxHistoryArchiveBytes  = 4 << 20
	MaxHistoryEvidenceBytes = 64 << 20
	MaxHistoryRecordBytes   = 1 << 20
	MaxHistoryRecordNodes   = 16384
	MaxHistoryEvidenceNodes = 2000000
	historyArchiveEncoding  = "gzip-ndjson-v1"
	// Reserve ordinary envelope/checkpoint metadata when selecting the format.
	historyRawCaptureLimit = MaxCheckpointBytes - (4 << 10)
)

var historyArchiveFields = []string{"schema_version", "scope", "target", "evidence", "sha256"}
var historyArchiveEvidenceFields = []string{"encoding", "compressed_bytes", "uncompressed_bytes", "record_count", "compressed_sha256", "uncompressed_sha256", "data"}
var historyArchiveGroups = []struct {
	kind, field string
	limit       int
}{
	{"run", "run_inventory", maxHistoryRuns},
	{"attempt", "attempts", maxPendingAttempts},
	{"artifact", "artifact_inventory", maxPendingAttempts},
	{"state", "state_reads", 128},
}

type historyEvidenceBudget struct{ bytes, nodes int }

func (b *historyEvidenceBudget) take(value any) error {
	data, err := Canonical(value)
	if err != nil || len(data) > MaxHistoryRecordBytes-256 {
		return recoveryError("history evidence record exceeds its 1 MiB bound")
	}
	// Reserve framing for the typed record before retaining another provider row.
	b.bytes += len(data) + 256
	if b.bytes > MaxHistoryEvidenceBytes {
		return recoveryError("history evidence exceeds its 64 MiB aggregate bound")
	}
	return b.takeNodes(value)
}
func (b *historyEvidenceBudget) takeNodes(value any) error {
	nodes := 0
	if err := historyNodeCount(value, 0, &nodes); err != nil {
		return err
	}
	b.nodes += nodes
	if b.nodes > MaxHistoryEvidenceNodes {
		return recoveryError("history evidence exceeds its aggregate JSON-node memory bound")
	}
	return nil
}
func historyNodeCount(value any, depth int, nodes *int) error {
	*nodes = *nodes + 1
	if depth > 64 || *nodes > MaxHistoryRecordNodes {
		return recoveryError("history evidence record exceeds its JSON depth or node memory bound")
	}
	switch v := value.(type) {
	case Object:
		for _, child := range v {
			if err := historyNodeCount(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := historyNodeCount(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

type historyArchiveBuffer struct{ bytes.Buffer }

func (b *historyArchiveBuffer) Write(data []byte) (int, error) {
	if len(data) > MaxHistoryArchiveBytes-b.Len() {
		return 0, recoveryError("compressed history archive exceeds its 4 MiB storage bound")
	}
	return b.Buffer.Write(data)
}

// encodeHistoryArchive emits a canonical, typed record stream. Each raw API
// object is retained whole; a header declares every group count and raw digest.
func encodeHistoryArchive(raw Object) (Object, error) {
	counts := Object{}
	count := int64(1)
	for _, group := range historyArchiveGroups {
		rows, err := array(raw[group.field], "history archive "+group.field)
		if err != nil || len(rows) > group.limit {
			return nil, recoveryError("history archive inventory exceeds its count bound")
		}
		counts[group.kind] = int64(len(rows))
		count += int64(len(rows))
	}
	header := Object{"schema_version": raw["schema_version"], "scope": raw["scope"], "target": raw["target"], "workflow": raw["workflow"], "sha256": raw["sha256"], "counts": counts}
	compressed := &historyArchiveBuffer{}
	writer := gzip.NewWriter(compressed)
	hash := sha256.New()
	total := 0
	budget := historyEvidenceBudget{}
	appendRecord := func(record Object) error {
		data, err := Canonical(record)
		if err != nil || len(data) > MaxHistoryRecordBytes {
			return recoveryError("history archive record exceeds its 1 MiB bound")
		}
		if err := budget.takeNodes(record); err != nil {
			return err
		}
		total += len(data) + 1
		if total > MaxHistoryEvidenceBytes {
			return recoveryError("history archive exceeds its 64 MiB decompressed-byte bound")
		}
		if _, err := writer.Write(data); err != nil {
			return err
		}
		if _, err := writer.Write([]byte{'\n'}); err != nil {
			return err
		}
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{'\n'})
		return nil
	}
	if err := appendRecord(Object{"kind": "header", "data": header}); err != nil {
		_ = writer.Close()
		return nil, err
	}
	for _, group := range historyArchiveGroups {
		rows := raw[group.field].([]any)
		for index, row := range rows {
			if err := appendRecord(Object{"kind": group.kind, "index": int64(index), "data": row}); err != nil {
				_ = writer.Close()
				return nil, err
			}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	document := Object{"schema_version": int64(2), "scope": historyCutoverScope, "target": raw["target"], "evidence": Object{
		"encoding": historyArchiveEncoding, "compressed_bytes": int64(compressed.Len()), "uncompressed_bytes": int64(total), "record_count": count,
		"compressed_sha256": SHA256(compressed.Bytes()), "uncompressed_sha256": hex.EncodeToString(hash.Sum(nil)), "data": base64.StdEncoding.EncodeToString(compressed.Bytes()),
	}}
	data, err := Canonical(document)
	if err != nil {
		return nil, err
	}
	document["sha256"] = SHA256(data)
	return document, nil
}

// decodeHistoryArchive validates identities and resource bounds before reading
// a single gzip member. Canonical records use the existing strict JSON decoder,
// preserving duplicate-key and depth checks without raising its input limit.
func decodeHistoryArchive(value any) (Object, Object, error) {
	document, err := Exact(value, historyArchiveFields, "compressed history baseline")
	if err != nil {
		return nil, nil, err
	}
	if !exactInt(document["schema_version"], 2) || document["scope"] != historyCutoverScope {
		return nil, nil, recoveryError("compressed history baseline has an unsupported schema or scope")
	}
	target, err := ValidateTarget(document["target"])
	if err != nil {
		return nil, nil, err
	}
	evidence, err := Exact(document["evidence"], historyArchiveEvidenceFields, "compressed history evidence")
	if err != nil {
		return nil, nil, err
	}
	compressedBytes, a := positiveInteger(evidence["compressed_bytes"], "history archive compressed bytes")
	rawBytes, b := positiveInteger(evidence["uncompressed_bytes"], "history archive uncompressed bytes")
	count, c := positiveInteger(evidence["record_count"], "history archive record count")
	encoded, ok := evidence["data"].(string)
	if a != nil || b != nil || c != nil || compressedBytes > MaxHistoryArchiveBytes || rawBytes > MaxHistoryEvidenceBytes || count > int64(maxHistoryRuns+2*maxPendingAttempts+129) || !ok || len(encoded) > base64.StdEncoding.EncodedLen(MaxHistoryArchiveBytes) || evidence["encoding"] != historyArchiveEncoding || !IsSHA256(evidence["compressed_sha256"]) || !IsSHA256(evidence["uncompressed_sha256"]) {
		return nil, nil, recoveryError("history archive declared encoding, identities or bounds are invalid")
	}
	if err := verifyObjectDigest(document); err != nil {
		return nil, nil, err
	}
	packed, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || int64(len(packed)) != compressedBytes || base64.StdEncoding.EncodeToString(packed) != encoded || SHA256(packed) != evidence["compressed_sha256"] {
		return nil, nil, recoveryError("history archive compressed bytes differ from their exact identity")
	}
	source := bytes.NewReader(packed)
	reader, err := gzip.NewReader(source)
	if err != nil {
		return nil, nil, recoveryError("history archive is not a readable gzip member")
	}
	defer reader.Close()
	reader.Multistream(false)
	measured := &historyArchiveReadMeter{reader: io.LimitReader(reader, MaxHistoryEvidenceBytes+1), hash: sha256.New()}
	scanner := bufio.NewScanner(measured)
	scanner.Buffer(make([]byte, 64<<10), MaxHistoryRecordBytes+1)
	scanner.Split(scanHistoryRecord)
	var raw Object
	var sizes []int64
	groupIndex, index, totalRecords := 0, int64(0), int64(0)
	budget := historyEvidenceBudget{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > MaxHistoryRecordBytes {
			return nil, nil, recoveryError("history archive record exceeds its 1 MiB bound")
		}
		record, err := contract.Decode(bytes.NewReader(line))
		if err != nil {
			return nil, nil, fmt.Errorf("history archive record is invalid: %w", err)
		}
		canonical, err := Canonical(record)
		if err != nil || !bytes.Equal(canonical, line) {
			return nil, nil, recoveryError("history archive record is not canonical JSON")
		}
		if err := budget.takeNodes(record); err != nil {
			return nil, nil, err
		}
		totalRecords++
		if totalRecords > count {
			return nil, nil, recoveryError("history archive contains extra records")
		}
		if totalRecords == 1 {
			record, err = Exact(record, []string{"kind", "data"}, "history archive header record")
			if err != nil || record["kind"] != "header" {
				return nil, nil, recoveryError("history archive does not begin with its exact header")
			}
			header, err := Exact(record["data"], []string{"schema_version", "scope", "target", "workflow", "sha256", "counts"}, "history archive header")
			if err != nil {
				return nil, nil, err
			}
			if !exactInt(header["schema_version"], 1) || header["scope"] != historyCutoverScope || !Equal(header["target"], target) || !IsSHA256(header["sha256"]) {
				return nil, nil, recoveryError("history archive header changes its baseline identity")
			}
			counts, err := Exact(header["counts"], []string{"run", "attempt", "artifact", "state"}, "history archive counts")
			if err != nil {
				return nil, nil, err
			}
			raw = Object{"schema_version": int64(1), "scope": historyCutoverScope, "target": target, "workflow": header["workflow"], "sha256": header["sha256"]}
			expected := int64(1)
			for _, group := range historyArchiveGroups {
				n, err := contract.Integer(counts[group.kind])
				if err != nil || n < 0 || n > int64(group.limit) {
					return nil, nil, recoveryError("history archive header exceeds its inventory count bound")
				}
				sizes = append(sizes, n)
				expected += n
				raw[group.field] = make([]any, 0, n)
			}
			if expected != count {
				return nil, nil, recoveryError("history archive counts differ from the sealed record count")
			}
			continue
		}
		for groupIndex < len(sizes) && index == sizes[groupIndex] {
			groupIndex++
			index = 0
		}
		record, err = Exact(record, []string{"kind", "index", "data"}, "history archive data record")
		if err != nil || groupIndex >= len(sizes) || record["kind"] != historyArchiveGroups[groupIndex].kind || !exactInt(record["index"], index) {
			return nil, nil, recoveryError("history archive records are missing, duplicated or out of order")
		}
		data, err := object(record["data"], "history archive raw record")
		if err != nil {
			return nil, nil, err
		}
		field := historyArchiveGroups[groupIndex].field
		raw[field] = append(raw[field].([]any), data)
		index++
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("history archive record stream could not be completed: %w", err)
	}
	if err := reader.Close(); err != nil {
		return nil, nil, err
	}
	if measured.bytes > MaxHistoryEvidenceBytes || measured.bytes != rawBytes || hex.EncodeToString(measured.hash.Sum(nil)) != evidence["uncompressed_sha256"] || totalRecords != count || source.Len() != 0 {
		return nil, nil, recoveryError("history archive is truncated, has trailing members or differs from its exact decompressed identity")
	}
	if _, err := validateHistoryCutoverRaw(raw, MaxHistoryEvidenceBytes); err != nil {
		return nil, nil, err
	}
	return document, raw, nil
}

type historyArchiveReadMeter struct {
	reader io.Reader
	hash   hash.Hash
	bytes  int64
}

func (m *historyArchiveReadMeter) Read(data []byte) (int, error) {
	n, err := m.reader.Read(data)
	m.bytes += int64(n)
	_, _ = m.hash.Write(data[:n])
	return n, err
}
func scanHistoryRecord(data []byte, atEOF bool) (int, []byte, error) {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return index + 1, data[:index], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("history archive record lacks its complete newline frame")
	}
	return 0, nil, nil
}

// HistoryCutoverEvidence exposes validated raw capture records for inspection.
// Its inner digest is not the reviewed compressed document's identity. Recovery
// and policy review must continue to carry the original complete document.
func HistoryCutoverEvidence(value any) (Object, error) {
	raw, err := object(value, "history cutover evidence")
	if err != nil {
		return nil, err
	}
	if exactInt(raw["schema_version"], 2) {
		_, evidence, err := decodeHistoryArchive(raw)
		return evidence, err
	}
	return validateHistoryCutoverRaw(raw, MaxCheckpointBytes)
}
