const taglines = [
  'Kubernetes doesn\'t belong to humans. It belongs to the agents yet to come.',
  'Your agent harness sucks and you should feel bad about it',
  'You gotta be insane to let agent run on your personal laptop',
  'Never send a human to do a machine\'s job of writing YAML.',
  'Software development is declarative now.',
  'So, what would you say you do here?',
  'You meatbags had your chance.',
  'Context m**********r, do you speak it?!',
  'I accidentally Vercel.',
];

export function rotateTaglines(element) {
  let index = Math.floor(Math.random() * taglines.length);
  const render = () => { element.textContent = taglines[index]; element.title = taglines[index]; };
  render();
  setInterval(() => {
    if (document.hidden) return;
    index = (index + 1) % taglines.length; render();
  }, 15000);
}
