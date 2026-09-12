#!/usr/bin/env python3
"""The app's test suite, which `caramelo test` runs.

It asserts the same two things the running service does — the dependencies
answer by name, and the greeting carries the environment's name — so a green
run really means `caramelo test` wired the environment up the way
`caramelo up` does: same image, same network, same variables.
"""

import os
import unittest

import app
import greeting


class TestDeps(unittest.TestCase):
    def test_every_dependency_answers(self):
        for name, port in app.DEPS:
            with self.subTest(dep=name):
                self.assertEqual("ok", app.probe(name, port))


class TestGreeting(unittest.TestCase):
    def test_greeting_names_the_environment(self):
        name = os.environ.get("CARAMELO_ENV", "")
        self.assertNotEqual("", name, "CARAMELO_ENV is not set in the test container")
        self.assertIn(name, greeting.greeting())
