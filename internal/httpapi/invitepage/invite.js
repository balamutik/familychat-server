const button = document.getElementById('open-app');
if (button) {
  const link = new URL('familychat://invite');
  link.searchParams.set('url', button.dataset.invite);
  button.href = link.href;
  // Browsers can require a gesture. Keep the explicit button and store link visible.
  const status = document.getElementById('status');
  status.textContent = 'Если приложение не открылось, нажмите кнопку выше.';
  window.location.href = link.href;
}
