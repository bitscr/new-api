/*
Copyright (C) 2026 bitscr

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For licensing inquiries, please open an issue at https://github.com/bitscr/new-api/issues
*/

import { useState, useCallback } from 'react';

const KEY = 'default_collapse_sidebar';

export const useSidebarCollapsed = () => {
  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem(KEY) === 'true',
  );

  const toggle = useCallback(() => {
    setCollapsed((prev) => {
      const next = !prev;
      localStorage.setItem(KEY, next.toString());
      return next;
    });
  }, []);

  const set = useCallback((value) => {
    setCollapsed(value);
    localStorage.setItem(KEY, value.toString());
  }, []);

  return [collapsed, toggle, set];
};
