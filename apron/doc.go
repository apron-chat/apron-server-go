// Package apron is a server for the Apron chat protocol
// (https://github.com/shazow/apron/blob/main/PROTOCOL.md), the reference
// implementation of every capability it defines. Open starts a Server
// from a Config, Handler serves it over HTTP and WebSocket, and Shutdown
// stops it. Package aprond runs it as the aprond command does, with its
// flags and config file.
//
// # Extensions
//
// Extensions add features without a fork. Each is an Extension in
// Config.Extensions, whose Init registers what it adds with its Host:
//
//   - commands (Host.Command) and request methods (Host.Method);
//   - a capability and its data in the server frame (Host.Advertise);
//   - HTTP routes (Host.Handle);
//   - filters that change or refuse messages before they are saved
//     (Host.FilterMessage);
//   - bots, which are in-process connections that speak the protocol
//     (Host.Session), and stream live text (Session.StreamAppend);
//   - followers of the log, live or replayed (Host.OnEvent,
//     Host.OnEventFrom), with DecodeMessage to read messages;
//   - external sign-in, such as OAuth (Host.ExternalSignIn);
//   - values kept in the store (Host.Store) and goroutines that run until
//     Shutdown (Host.Go).
//
// Extension code never runs while the server holds its lock, so it may
// call any Host or Session method. What it sees of users and rooms are
// copies (User, Room); Host.Visible tells whether someone may see a room,
// which an extension that shows messages checks.
//
// The servers in the examples directory are complete extensions with
// tests.
package apron
