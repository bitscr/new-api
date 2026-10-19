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

import React from 'react';
import { Button } from '@douyinfe/semi-ui';
import { TrendingUp } from 'lucide-react';

// 顶栏「模型得分」入口:仅登录后显示,点击进入 /console/model-score。
const ModelScoreButton = ({ userState, navigate, t }) => {
  if (!userState?.user) return null;

  return (
    <Button
      theme='borderless'
      type='tertiary'
      icon={<TrendingUp size={18} />}
      aria-label={t('模型得分')}
      onClick={() => navigate('/console/model-score')}
      className='!px-2 !py-1.5 !text-current focus:!bg-semi-color-fill-1 !rounded-full !bg-semi-color-fill-0 hover:!bg-semi-color-fill-1'
    >
      <span className='hidden lg:inline text-xs font-medium'>
        {t('模型得分')}
      </span>
    </Button>
  );
};

export default ModelScoreButton;
