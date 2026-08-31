from __future__ import annotations

import base64
import json
import unittest

from app.oauth import extract_jwt_email, extract_user_email


class OAuthIdentityTests(unittest.TestCase):
    def test_extract_user_email_supports_nested_fields(self):
        """OAuth 使用者資料的巢狀郵箱欄位應可被辨識。"""
        user = {"profile": {"emailAddress": "nested@example.com"}}

        self.assertEqual(extract_user_email(user), "nested@example.com")

    def test_extract_jwt_email_is_a_fallback(self):
        """使用者資料缺失時，應可從 JWT claims 讀取郵箱。"""
        claims = base64.urlsafe_b64encode(json.dumps({"email": "jwt@example.com"}).encode()).decode().rstrip("=")
        token = f"header.{claims}.signature"

        self.assertEqual(extract_jwt_email(token), "jwt@example.com")


if __name__ == "__main__":
    unittest.main()
