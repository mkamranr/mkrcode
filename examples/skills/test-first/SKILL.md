---
name: test-first
description: Use when adding a feature or fixing a bug, to write a failing test before the implementation and prove the test actually detects the problem.
---

# Test first

A test written after the implementation usually proves only that the code does
what it does. Write it first, and watch it fail for the right reason.

## Steps

1. **Find how this project tests.** Look for existing test files near the code
   you are changing and follow their conventions — do not introduce a new
   framework or style.

2. **Write one failing test** that captures the behaviour being added, or the
   bug being fixed. Name it for the behaviour, not the function.

3. **Run it and read the failure.** Use `exec` to run only this test. The
   failure message must describe the actual problem. If it fails for an
   unrelated reason — a typo, a missing import, a wrong path — fix that and run
   again. A test that passes immediately is testing nothing; say so and
   reconsider it.

4. **Write the smallest implementation** that makes it pass.

5. **Run the whole suite**, not just the new test, and report the result
   honestly including anything that broke.

## For a bug fix

Reproduce the bug in the test first. If you cannot make the test fail before
the fix, you have not understood the bug yet — investigate further rather than
changing code hopefully.
