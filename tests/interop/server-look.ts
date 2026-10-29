/**
 * How aprond shows itself in the interop tests (playwright.config.ts starts it with --title, --description and
 * --avatar), so reference.spec.ts can check the web client shows the server by them (§3.1).
 */
export const SERVER_TITLE = 'Interop Lab';
export const SERVER_DESCRIPTION = 'Browser tests against the reference server.';
/** A 1x1 PNG: the server takes a data:image URL of at most 64 KiB as its avatar, as for a user's. */
export const SERVER_AVATAR = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';
