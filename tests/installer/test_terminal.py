"""Exercise real controlling-terminal input, including piped installation."""
import errno
import os
from pathlib import Path
import pty
import select
import signal
import sys
import time
import unittest

SCRIPT = Path(__file__).resolve().parents[2] / 'scripts/familychat.py'


class TerminalTests(unittest.TestCase):
    def dialogue(self, body, answers):
        code = (f'import os, runpy; module = runpy.run_path({str(SCRIPT)!r}); '
                'fd = os.open(os.devnull, os.O_RDONLY); os.dup2(fd, 0); os.close(fd); ' + body)
        pid, master = pty.fork()
        if pid == 0:
            os.execv(sys.executable, [sys.executable, '-c', code])
        output = b''
        sent = 0
        status = None
        deadline = time.monotonic() + 8
        try:
            while time.monotonic() < deadline:
                ready, _, _ = select.select([master], [], [], 0.1)
                if ready:
                    try:
                        chunk = os.read(master, 8192)
                    except OSError as error:
                        if error.errno != errno.EIO:
                            raise
                        break
                    if not chunk:
                        break
                    output += chunk
                    if sent < len(answers):
                        marker, occurrence, answer = answers[sent]
                        if output.count(marker) >= occurrence:
                            os.write(master, answer)
                            sent += 1
                if status is None:
                    done, status_value = os.waitpid(pid, os.WNOHANG)
                    if done:
                        status = status_value
            return output
        finally:
            if status is None:
                done, status_value = os.waitpid(pid, os.WNOHANG)
                if not done:
                    try:
                        os.kill(pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    os.waitpid(pid, 0)
            os.close(master)

    def test_prompts_read_controlling_terminal_when_stdin_is_redirected(self):
        output = self.dialogue(
            'first = module["prompt"]("Domain:"); second = module["prompt"]("Email:"); '
            'print("RESULT=" + first + "|" + second, flush=True)',
            [(b'Domain:', 1, b'chat.example.com\n'), (b'Email:', 1, b'admin@example.com\n')])
        self.assertIn(b'RESULT=chat.example.com|admin@example.com', output, output.decode(errors='replace'))

    def test_invalid_utf8_requires_a_fresh_answer_and_does_not_accept_partial_yes(self):
        # A terminal without IUTF8 can leave the first byte of Cyrillic н after
        # Backspace. The y after that byte must not count as approval.
        output = self.dialogue(
            'print("RESULT=" + module["prompt"]("Confirm:"), flush=True)',
            [(b'Confirm: ', 1, b'\xd0y\n'), (b'Confirm: ', 2, b'n\n')])
        self.assertIn(b'RESULT=n', output, output.decode(errors='replace'))
        self.assertNotIn(b'RESULT=y', output)
        self.assertNotIn(b'UnicodeDecodeError', output)

    def test_end_of_input_aborts_instead_of_retrying(self):
        output = self.dialogue('module["prompt"]("Confirm:")', [(b'Confirm: ', 1, b'\x04')])
        self.assertIn('Ввод прерван.'.encode('utf-8'), output, output.decode(errors='replace'))

    def test_valid_cyrillic_answer_is_preserved(self):
        output = self.dialogue(
            'print("RESULT=" + module["prompt"]("Confirm:"), flush=True)',
            [(b'Confirm: ', 1, 'да\n'.encode('utf-8'))])
        self.assertIn('RESULT=да'.encode('utf-8'), output, output.decode(errors='replace'))


if __name__ == '__main__':
    unittest.main()
