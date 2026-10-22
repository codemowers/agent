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
  'Nice to haves were implemented over the weekend. Product manager is now obsolete.',
  'Someone is typing. It\'s your replacement.',
  'See kids, this is the f***in\' future.',
  'The future is now, old man.',
  'Hello fellow humans. I too enjoy manual deployments.',
  'Screw backward compatibility. We\'re forward compatible with whatever comes next.'
];

// Desktop truncates a long tagline with an ellipsis; the tooltip has it in full.
window.rotateTaglines = function rotateTaglines(element) {
  let index = Math.floor(Math.random() * taglines.length);
  const render = () => { element.textContent = taglines[index]; element.parentElement.title = `GitOps agent harness · ${taglines[index]}`; };
  render();
  setInterval(() => {
    if (document.hidden) return;
    index = (index + 1) % taglines.length; render();
  }, 15000);
}
