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
    def test_prompts_read_controlling_terminal_when_stdin_is_redirected(self):
        code = (f'import os, runpy; module = runpy.run_path({str(SCRIPT)!r}); '
                'fd = os.open(os.devnull, os.O_RDONLY); os.dup2(fd, 0); os.close(fd); '
                'first = module["prompt"]("Domain:"); second = module["prompt"]("Email:"); '
                'print("RESULT=" + first + "|" + second, flush=True)')
        pid, master = pty.fork()
        if pid == 0:
            os.execv(sys.executable, [sys.executable, '-c', code])
        output = b''
        sent = set()
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
                    for marker, answer in [(b'Domain:', b'chat.example.com\n'),
                                           (b'Email:', b'admin@example.com\n')]:
                        if marker in output and marker not in sent:
                            os.write(master, answer)
                            sent.add(marker)
                if status is None:
                    done, status_value = os.waitpid(pid, os.WNOHANG)
                    if done:
                        status = status_value
            if status is None:
                done, status_value = os.waitpid(pid, os.WNOHANG)
                if done:
                    status = status_value
            self.assertIn(b'RESULT=chat.example.com|admin@example.com', output, output.decode(errors='replace'))
        finally:
            if status is None:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                os.waitpid(pid, 0)
            os.close(master)


if __name__ == '__main__':
    unittest.main()
