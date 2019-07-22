// Package rcon implements Source RCON Protocol which is described in the
// documentation: https://developer.valvesoftware.com/wiki/Source_RCON_Protocol
package rcon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	// MaxCommandLen is an artificial restriction, but it will help in case of random
	// large queries.
	MaxCommandLen = 1000

	// SERVERDATA_AUTH is the first packet sent by the client,
	// which is used to authenticate the conn with the server.
	SERVERDATA_AUTH int32 = 3

	// SERVERDATA_AUTH_ID is any positive integer, chosen by the client
	// (will be mirrored back in the server's response).
	// Conan Exiles has a bug because of which it always responds 42
	// regardless of the value of the request ID.
	SERVERDATA_AUTH_ID int32 = 42

	// SERVERDATA_AUTH_RESPONSE packet is a notification of the conn's current auth
	// status. When the server receives an auth request, it will respond with an empty
	// SERVERDATA_RESPONSE_VALUE, followed immediately by a SERVERDATA_AUTH_RESPONSE
	// indicating whether authentication succeeded or failed. Note that the status
	// code is returned in the packet id field, so when pairing the response with
	// the original auth request, you may need to look at the packet id of the
	// preceding SERVERDATA_RESPONSE_VALUE.
	// If authentication was successful, the ID assigned by the request.
	// If auth failed, -1 (0xFF FF FF FF).
	SERVERDATA_AUTH_RESPONSE int32 = 2

	// SERVERDATA_RESPONSE_VALUE packet is the response to a SERVERDATA_EXECCOMMAND
	// request. The ID assigned by the original request.
	SERVERDATA_RESPONSE_VALUE int32 = 0

	// SERVERDATA_EXECCOMMAND packet type represents a command issued to the server
	// by a client. The response will vary depending on the command issued.
	SERVERDATA_EXECCOMMAND int32 = 2

	// SERVERDATA_EXECCOMMAND_ID is any positive integer, chosen by the client
	// (will be mirrored back in the server's response).
	// Conan Exiles has a bug because of which it always responds 42
	// regardless of the value of the request ID.
	SERVERDATA_EXECCOMMAND_ID int32 = 42
)

var (
	// ErrInvalidAuthResponse is returned when we didn't get an auth packet
	// back for second read try after discard empty SERVERDATA_RESPONSE_VALUE
	// from authentication response.
	ErrInvalidAuthResponse = errors.New("invalid authentication packet type response")

	// ErrAuthFailed is returned when the package id from authentication
	// response is -1.
	ErrAuthFailed = errors.New("authentication failed")

	// ErrInvalidPacketID is returned when the package id from server response
	// was not mirrored back from request.
	ErrInvalidPacketID = errors.New("response for another request")

	// ErrInvalidPacketPadding is returned when the bytes after type field from
	// response is not equal to null-terminated ASCII strings.
	ErrInvalidPacketPadding = errors.New("invalid response padding")

	// ErrResponseTooSmall is returned when the server response is smaller
	// than 10 bytes.
	ErrResponseTooSmall = errors.New("response too small")

	// ErrCommandTooLong is returned when executed command length is bigger
	// than MaxCommandLen characters.
	ErrCommandTooLong = errors.New("command too long")
)

// Conn is source RCON generic stream-oriented network connection.
type Conn struct {
	conn net.Conn
}

// Dial creates a new authorized Conn tcp dialer connection.
func Dial(address string, password string) (*Conn, error) {
	const timeout = 10 * time.Second
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		// Failed to open TCP conn to the server.
		return nil, err
	}

	client := &Conn{conn: conn}

	if err := client.auth(password); err != nil {
		// Failed to auth conn with the server.
		if err2 := client.Close(); err2 != nil {
			return client, fmt.Errorf("an error occurred while handling another error: %s. Previous error: %s", err2, err)
		}

		return client, err
	}

	return client, nil
}

// Execute sends command type and it string to execute to the remote server,
// creating a packet with a SERVERDATA_EXECCOMMAND_ID for the server to mirror,
// and compiling its payload bytes in the appropriate order. The response body
// is decompiled from bytes into a string for return.
func (c *Conn) Execute(command string) (string, error) {
	if len(command) > MaxCommandLen {
		return "", ErrCommandTooLong
	}

	if _, err := c.write(SERVERDATA_EXECCOMMAND, SERVERDATA_EXECCOMMAND_ID, command); err != nil {
		return "", err
	}

	response, err := c.read(30 * time.Second)
	if err != nil {
		return response.Body(), err
	}

	if response.ID != SERVERDATA_EXECCOMMAND_ID {
		return response.Body(), ErrInvalidPacketID
	}

	return response.Body(), nil
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Close closes the connection.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// auth sends SERVERDATA_AUTH request to the remote server and
// authenticates client for the next requests.
func (c *Conn) auth(password string) error {
	timeout := 10 * time.Second

	if _, err := c.write(SERVERDATA_AUTH, SERVERDATA_AUTH_ID, password); err != nil {
		return err
	}

	response, err := c.readHeader(timeout)
	if err != nil {
		return err
	}

	// When the server receives an auth request, it will respond with an empty
	// SERVERDATA_RESPONSE_VALUE, followed immediately by a SERVERDATA_AUTH_RESPONSE
	// indicating whether authentication succeeded or failed.
	// Some servers doesn't send an empty SERVERDATA_RESPONSE_VALUE packet, so we
	// do this case optional.
	if response.Type == SERVERDATA_RESPONSE_VALUE {
		// Discard empty SERVERDATA_RESPONSE_VALUE from authentication response.
		c.conn.Read(make([]byte, response.Size-int32(PacketHeaderSize)))

		if response, err = c.readHeader(timeout); err != nil {
			return err
		}
	}

	// We must to read response body.
	buffer := make([]byte, response.Size-int32(PacketHeaderSize))
	if _, err := c.conn.Read(buffer); err != nil {
		return err
	}

	if response.Type != SERVERDATA_AUTH_RESPONSE {
		return ErrInvalidAuthResponse
	}

	if response.ID == -1 {
		return ErrAuthFailed
	}

	if response.ID != SERVERDATA_AUTH_ID {
		return ErrInvalidPacketID
	}

	return nil
}

// Write creates packet and writes it to established tcp conn.
func (c *Conn) write(packetType int32, packetID int32, command string) (int64, error) {
	packet := NewPacket(packetType, packetID, command)

	return packet.WriteTo(c.conn)
}

// readHeader reads structured binary data without body from c.conn into packet.
func (c *Conn) readHeader(timeout time.Duration) (Packet, error) {
	c.conn.SetReadDeadline(time.Now().Add(timeout))

	var packet Packet
	if err := binary.Read(c.conn, binary.LittleEndian, &packet.Size); err != nil {
		return packet, err
	}

	if err := binary.Read(c.conn, binary.LittleEndian, &packet.ID); err != nil {
		return packet, err
	}

	if err := binary.Read(c.conn, binary.LittleEndian, &packet.Type); err != nil {
		return packet, err
	}

	return packet, nil
}

// read reads structured binary data from c.conn into packet.
func (c *Conn) read(timeout time.Duration) (*Packet, error) {
	c.conn.SetReadDeadline(time.Now().Add(timeout))

	packet := &Packet{}
	if _, err := packet.ReadFrom(c.conn); err != nil {
		return packet, err
	}

	// Workaround for Rust server. Rust rcon server responses packet with a
	// type of 4 and the next packet is valid. It is undocumented, so skip
	// packet and read next.
	if packet.Type == 4 {
		if _, err := packet.ReadFrom(c.conn); err != nil {
			return packet, err
		}
	}

	return packet, nil
}
