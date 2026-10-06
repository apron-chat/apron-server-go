import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig, devices } from '@playwright/test';
import { TEST_VAPID_PRIVATE_KEY, TEST_VAPID_SUBJECT } from './push-test-vapid';

const configDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(configDirectory, '../..');
const chromiumExecutablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH;
const webDirectory = process.env.APRON_WEB_DIR ? path.resolve(process.env.APRON_WEB_DIR) : path.join(repositoryRoot, '.apron-web');
// The push flags serve push.spec.ts and change nothing for the other tests, which register no
// push endpoint: a fixed VAPID key, whose public half the test checks tokens against, and
// --push.allow-insecure, so the test's capture endpoint on http://127.0.0.1 is accepted.
const aprondFlags = [
	'--store memory',
	'--addr 127.0.0.1:8080',
	'--push.allow-insecure',
	`--push.vapid-private-key ${TEST_VAPID_PRIVATE_KEY}`,
	`--push.vapid-subject ${TEST_VAPID_SUBJECT}`
].join(' ');
// TODO: Make server ports configurable so browser tests can run beside local development servers.

export default defineConfig({
	testDir: configDirectory,
	testMatch: /\.spec\.ts$/,
	fullyParallel: false,
	workers: 1,
	retries: process.env.CI ? 1 : 0,
	timeout: 30_000,
	expect: {
		timeout: 10_000
	},
	reporter: process.env.CI
		? [['line'], ['html', { open: 'never' }]]
		: 'list',
	use: {
		baseURL: 'http://127.0.0.1:5173',
		trace: 'retain-on-failure',
		screenshot: 'only-on-failure'
	},
	projects: [
		{
			name: 'desktop',
			testMatch: /(chat|webauthn|reference)\.spec\.ts$/,
			use: {
				...devices['Desktop Chrome'],
				...(chromiumExecutablePath
					? { launchOptions: { executablePath: chromiumExecutablePath } }
					: {})
			}
		},
		{
			name: 'mobile',
			testMatch: /responsive\.spec\.ts$/,
			use: {
				...devices['Pixel 5'],
				...(chromiumExecutablePath
					? { launchOptions: { executablePath: chromiumExecutablePath } }
					: {})
			}
		},
		{
			// Web push (§4.9) and status (§4.5).
			name: 'push',
			testMatch: /push\.spec\.ts$/,
			// A connection turns idle only after 30 seconds unattended (§4.5), and the tests wait for it.
			timeout: 180_000,
			use: {
				...devices['Desktop Chrome'],
				// Passkeys need the default RP ID, localhost.
				baseURL: 'http://localhost:5173',
				// Full Chromium in its new headless mode: the headless shell reports
				// Notification.permission as denied whatever is granted.
				...(chromiumExecutablePath
					? { launchOptions: { executablePath: chromiumExecutablePath } }
					: { channel: 'chromium' })
			}
		}
	],
	webServer: [
		{
			command: `go run ./cmd/aprond ${aprondFlags}`,
			cwd: repositoryRoot,
			url: 'http://127.0.0.1:8080/healthz',
			timeout: 120_000,
			reuseExistingServer: false
		},
		{
			command: 'npm run dev -- --host 127.0.0.1 --port 5173 --strictPort',
			cwd: webDirectory,
			url: 'http://127.0.0.1:5173',
			timeout: 120_000,
			reuseExistingServer: false
		}
	]
});
