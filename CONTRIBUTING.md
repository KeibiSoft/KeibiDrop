# Contributing to KeibiDrop

Thanks for considering contributing to **KeibiDrop**, maintained by **KeibiSoft SRL**. We welcome improvements, fixes, and features—especially if they're clean, well-scoped, and don’t turn the codebase into spaghetti.

Before opening a pull request, please read this carefully. We care about licensing, structure, and not going insane during code review.

**🚨 All commits must be signed using `git commit -S`.  
By submitting a signed commit, you agree to the terms in [DCO.txt](./DCO.txt).**  
This includes:
- Declaring you have the right to submit the code
- Agreeing to license your contribution under MPL 2.0
- Allowing KeibiSoft SRL to dual-license your contribution **if** it modifies KeibiSoft-authored code

---

## Contribution Guidelines

### Prefer Self-Contained Modules

If you want to add new features, we strongly prefer **modular pull requests**. These are self-contained additions—like a new feature module, plugin, or helper package—that don't modify core files directly.

- You keep full control and copyright over these modules.
- We will **not dual-license** your code without your explicit permission.
- You’re free to license your module however you want, as long as it works with MPL 2.0.

**Why?** Because we want to make contributions easy without ownership ambiguity.

---

### Fixing or Modifying Core KeibiSoft Code

If you're submitting:
- Bug fixes
- Patches
- Performance improvements
- Modifications to **code originally written by KeibiSoft**

Then by submitting, you agree to allow **KeibiSoft SRL** to:
- Re-license that contribution under a **dual-license** (including a commercial license), in addition to MPL 2.0.

This keeps the project open-source *and* commercially viable.

If you don’t want to allow that, no problem—just don’t change KeibiSoft-authored modules. Consider writing your own extension or module instead.

---

## Commit Signing (Required)

**All commits must be signed.**  
Unsigned commits will be rejected without review. This is for authenticity, auditability, and because unsigned commits are a vibe kill.

To sign your commits:

```bash
git commit -S -m "your message here"
```

Set up GPG or SSH signing first:
https://docs.github.com/en/authentication/managing-commit-signature-verification

## Licensing

KeibiDrop is licensed under the **Mozilla Public License 2.0 (MPL-2.0)**.

By contributing, you confirm that:
- You wrote the code yourself **or**
- You have the legal right to contribute it under **MPL 2.0**, and
- You agree to the dual-licensing terms **only if** your contribution modifies KeibiSoft-authored code

---

## How to Contribute

1. Fork the repository  
2. Create a feature branch: `git checkout -b feature/my-module`  
3. Make your changes (modular is better!)  
4. Sign your commits using `git commit -S -m "your message here"`  
5. Push your changes: `git push origin feature/my-module`  
6. Open a pull request with a clear, descriptive message

---

## 📬 Questions?

Open an issue or start a discussion thread. We try to respond in a human timeframe.

Thanks again for contributing. The community and caffeine both appreciate you.

– KeibiSoft SRL
