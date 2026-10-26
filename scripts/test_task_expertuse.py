"""限时活动「体验指定专家」四连（expert_use_*，valid_end 2026-11-20）。

这四条任务与 WorkBuddy_Valorant 同族，差别只在专家来源：valorant 靠市场按
agent_name 搜索，这四条由任务自身绑定 expertId（jump_url 带出）。钉住两点：
1) 绑定 id 不被市场搜索结果覆盖（搜索会命中同名的其它专家）；
2) 未绑定的 code 返回空，调用方据此保守跳过。
"""
import unittest

import task_runner as runner


class ExpertUseTest(unittest.TestCase):
    def test_bound_expert_id_used_directly(self) -> None:
        ids = runner.ids_for("expertuse", {"uid": "u1", "token": "t"}, 1,
                             code="expert_use_maidehao")
        self.assertEqual(len(ids), 1)
        self.assertEqual(ids[0][0], "ex_GDl31WahyIH0")
        self.assertEqual(ids[0][1]["name"], "卖得好")

    def test_all_four_bound(self) -> None:
        expected = {
            "expert_use_maidehao": "ex_GDl31WahyIH0",
            "expert_use_douyinyin": "ex_FbY5nlWTqDod",
            "expert_use_shangjia_xuanchuan": "ex_Weo3x2EZuSDm",
            "expert_use_bowangwang": "ex_WZY136wV5aMt",
        }
        for code, eid in expected.items():
            ids = runner.ids_for("expertuse", {}, 1, code=code)
            self.assertEqual([i for i, _ in ids], [eid], code)

    def test_unbound_code_yields_nothing(self) -> None:
        self.assertEqual(runner.ids_for("expertuse", {}, 1, code="not_mapped"), [])

    def test_actual_use_is_cloud_shape(self) -> None:
        expert = runner.EXPERT_USE_TASKS["expert_use_douyinyin"]
        ev = runner.desktop_expert_actual_use_cloud(expert, "conv", "cmb-" + "c3" * 16)
        self.assertEqual(ev["mode"], "CLOUD")
        self.assertEqual(ev["type"], "06-ContentCreative")
        self.assertEqual(ev["source"], "market")

    def test_all_four_are_limited_and_unexpired(self) -> None:
        for code in ("expert_use_maidehao", "expert_use_douyinyin",
                     "expert_use_shangjia_xuanchuan", "expert_use_bowangwang"):
            self.assertIn(code, runner.LIMITED_CODES)
            self.assertEqual(runner.MAPPING[code]["valid_end"], "2026-11-20T23:59:00+08:00")
            self.assertTrue(runner.within_valid_period(runner.MAPPING[code]))


if __name__ == "__main__":
    unittest.main()
