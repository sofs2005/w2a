"""AccountSpacing 的单元测试（与 Go scheduler 的账号随机间隔配套）.

直接跑：python scripts/test_account_spacing.py
"""
import argparse
import sys
import unittest
from unittest import mock

import task_common as tc


class AccountSpacingTest(unittest.TestCase):
    def test_first_account_has_no_wait_and_later_accounts_use_random_range(self):
        spacing = tc.AccountSpacing(30, 120)
        rng = mock.Mock()
        rng.uniform.side_effect = [31.5, 119.0]
        with mock.patch.object(tc.random, "SystemRandom", return_value=rng), \
                mock.patch.object(tc.time, "sleep") as sleep:
            spacing.wait()
            sleep.assert_not_called()
            spacing.wait()
            spacing.wait()
        self.assertEqual(sleep.call_args_list, [mock.call(31.5), mock.call(119.0)])
        self.assertEqual(rng.uniform.call_args_list, [mock.call(30, 120), mock.call(30, 120)])

    def test_default_spacing_does_not_sleep_or_draw(self):
        with mock.patch.object(tc.random, "SystemRandom") as rng, \
                mock.patch.object(tc.time, "sleep") as sleep:
            spacing = tc.AccountSpacing()
            spacing.wait()
            spacing.wait()
        rng.assert_not_called()
        sleep.assert_not_called()

    def test_cli_defaults_and_bad_ranges(self):
        parser = argparse.ArgumentParser()
        tc.add_account_spacing_arguments(parser)
        defaults = tc.account_spacing_from_args(parser.parse_args([]), parser)
        self.assertEqual((defaults.minimum, defaults.maximum), (0, 0))
        for minimum, maximum in [(-1, 120), (30, 29), (0, 601)]:
            with self.subTest(pair=(minimum, maximum)), self.assertRaises(ValueError):
                tc.AccountSpacing(minimum, maximum)


if __name__ == "__main__":
    unittest.main()
