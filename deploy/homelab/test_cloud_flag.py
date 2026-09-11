import os, subprocess, unittest
from pathlib import Path

class CloudFlagTest(unittest.TestCase):
    def run_flag(self, flag, key='test-only-not-a-real-key'):
        env = os.environ.copy()
        env.pop('FIRECRAWL_ENABLED', None)
        if flag is not None:
            env['FIRECRAWL_ENABLED'] = flag
        env['FIRECRAWL_CLOUD_API_KEY'] = key
        return subprocess.run(['sh', str(Path(__file__).with_name('router-entrypoint.sh')), 'sh', '-c', 'printf "%s" "$FIRECRAWL_CLOUD_API_KEY"'], env=env, capture_output=True, text=True)
    def test_disabled_removes_key(self):
        for flag in (None, 'false'):
            r = self.run_flag(flag)
            self.assertEqual(r.returncode, 0)
            self.assertEqual(r.stdout, '')
    def test_enabled_preserves_key(self):
        r = self.run_flag('true')
        self.assertEqual(r.returncode, 0)
        self.assertEqual(r.stdout, 'test-only-not-a-real-key')
    def test_enabled_requires_key(self):
        self.assertNotEqual(self.run_flag('true', '').returncode, 0)
    def test_invalid_fails_closed(self):
        self.assertNotEqual(self.run_flag('typo').returncode, 0)

if __name__ == '__main__': unittest.main()
