import { redirect } from '@sveltejs/kit';

// Moved under Configure; old links and bookmarks still land.
export const load = () => redirect(308, '/configure/users');
