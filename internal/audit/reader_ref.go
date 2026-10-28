package audit

import (
	"encoding/base64"
	"encoding/binary"
)

// RecordRef includes a pathname hint to locate an old record for detail reads.
// RecordPos remains the stable comparable identity used by the reducer: the
// hint changes when a live segment is archived and is never part of equality.
type RecordRef struct {
	Pos  RecordPos
	Hint uint64
}

const recordRefBytes = 1 + positionBytes + 8

func (ref RecordRef) String() string {
	var bytes [recordRefBytes]byte
	bytes[0] = 1
	raw, _ := base64.RawURLEncoding.DecodeString(ref.Pos.String())
	copy(bytes[1:1+positionBytes], raw)
	binary.BigEndian.PutUint64(bytes[1+positionBytes:], ref.Hint)
	return base64.RawURLEncoding.EncodeToString(bytes[:])
}

func ParseRecordRef(raw string) (RecordRef, error) {
	var ref RecordRef
	bytes, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(bytes) != recordRefBytes || bytes[0] != 1 {
		return ref, ErrInvalidCursor
	}
	pos, err := ParseRecordPos(base64.RawURLEncoding.EncodeToString(bytes[1 : 1+positionBytes]))
	if err != nil {
		return RecordRef{}, err
	}
	return RecordRef{Pos: pos, Hint: binary.BigEndian.Uint64(bytes[1+positionBytes:])}, nil
}
