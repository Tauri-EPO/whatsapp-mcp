package main

import (
	"context"
	"errors"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	historyShareMessageLimit = 5000
	// Includes every nested field and packed scalar element, bounding repeated
	// metadata, empty conversations and duplicate singular submessage merges.
	historyShareElementLimit = 100000
	historyShareDepthLimit   = 100
)

type historyShareWireCounts struct {
	messages     int
	messageInfos int
	elements     int
}

// Walk descriptors and slices of the bounded plaintext without allocating any
// peer-controlled proto objects or repeated-field slices. Unknown fields are
// skipped here and discarded by the actual decoder; groups are refused.
func scanHistoryShareWire(ctx context.Context, plain []byte) (historyShareWireCounts, error) {
	counts := historyShareWireCounts{}
	descriptor := (*waHistorySync.HistorySync)(nil).ProtoReflect().Descriptor()
	err := counts.scan(ctx, plain, descriptor, 0)
	return counts, err
}

func (c *historyShareWireCounts) addElement() error {
	c.elements++
	if c.elements > historyShareElementLimit {
		return errors.New("shared history wire element limit exceeded")
	}
	return nil
}

func (c *historyShareWireCounts) scan(ctx context.Context, wire []byte, descriptor protoreflect.MessageDescriptor, depth int) error {
	if depth >= historyShareDepthLimit {
		return errors.New("shared history wire depth limit exceeded")
	}
	if descriptor.FullName() == "WAWebProtobufsHistorySync.HistorySyncMsg" {
		c.messages++
		if c.messages > historyShareMessageLimit {
			return errors.New("shared history message limit exceeded")
		}
	}
	// Status rows and duplicate singular message fields can allocate full
	// WebMessageInfo objects without another HistorySyncMsg wrapper.
	if descriptor.FullName() == "WAWebProtobufsWeb.WebMessageInfo" {
		c.messageInfos++
		if c.messageInfos > historyShareMessageLimit {
			return errors.New("shared history message object limit exceeded")
		}
	}
	for len(wire) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.addElement(); err != nil {
			return err
		}
		number, kind, n := protowire.ConsumeTag(wire)
		if n < 0 {
			return protowire.ParseError(n)
		}
		wire = wire[n:]
		if kind == protowire.StartGroupType || kind == protowire.EndGroupType {
			return errors.New("shared history wire groups refused")
		}
		field := descriptor.Fields().ByNumber(number)
		if kind == protowire.BytesType {
			value, consumed := protowire.ConsumeBytes(wire)
			if consumed < 0 {
				return protowire.ParseError(consumed)
			}
			if field != nil && field.Kind() == protoreflect.MessageKind {
				if err := c.scan(ctx, value, field.Message(), depth+1); err != nil {
					return err
				}
			} else if field != nil && field.IsList() && field.Kind() != protoreflect.StringKind && field.Kind() != protoreflect.BytesKind {
				if err := c.packed(value, field.Kind()); err != nil {
					return err
				}
			}
			wire = wire[consumed:]
		} else {
			consumed := protowire.ConsumeFieldValue(number, kind, wire)
			if consumed < 0 {
				return protowire.ParseError(consumed)
			}
			wire = wire[consumed:]
		}
	}
	return ctx.Err()
}

func (c *historyShareWireCounts) packed(wire []byte, kind protoreflect.Kind) error {
	for len(wire) > 0 {
		if err := c.addElement(); err != nil {
			return err
		}
		var n int
		switch kind {
		case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
			_, n = protowire.ConsumeFixed32(wire)
		case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
			_, n = protowire.ConsumeFixed64(wire)
		default:
			_, n = protowire.ConsumeVarint(wire)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		wire = wire[n:]
	}
	return nil
}
