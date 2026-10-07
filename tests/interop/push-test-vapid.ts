/**
 * The VAPID key pair aprond signs Web Push with in the interop tests (playwright.config.ts), so
 * push.spec.ts can check tokens against its public half. It is a test fixture: it is public, so
 * never use it for a real server, which generates its own key or takes one from
 * --push.vapid-private-key.
 */
export const TEST_VAPID_PRIVATE_KEY = 'bXTr5nHrl6nDHBL4V4g_XLy6WRhXXezSvZ3Dm3_YilM';
export const TEST_VAPID_SUBJECT = 'mailto:interop@example.com';
