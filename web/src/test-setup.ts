// jsdom gaps that Kobalte touches.
Element.prototype.scrollIntoView ??= () => {};
window.scrollTo = () => {};
